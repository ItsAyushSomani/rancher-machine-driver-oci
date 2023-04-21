package oci

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"github.com/oracle/oci-go-sdk/example/helpers"
	"github.com/rancher/machine/libmachine/log"
	"io/ioutil"
	"net/http"
	"strings"
	"time"

	"github.com/oracle/oci-go-sdk/common"
	"github.com/oracle/oci-go-sdk/core"
	"github.com/oracle/oci-go-sdk/identity"
)

// Client defines / contains the OCI/Identity clients and operations.
type Client struct {
	configuration        common.ConfigurationProvider
	computeClient        core.ComputeClient
	virtualNetworkClient core.VirtualNetworkClient
	identityClient       identity.IdentityClient
	sleepDuration        time.Duration
	// TODO we could also include the retry settings here
}

func newClient(configuration common.ConfigurationProvider, d *Driver) (*Client, error) {

	computeClient, err := core.NewComputeClientWithConfigurationProvider(configuration)
	if err != nil {
		log.Debugf("create new Compute client failed with err %v", err)
		return nil, err
	}
	vNetClient, err := core.NewVirtualNetworkClientWithConfigurationProvider(configuration)
	if err != nil {
		log.Debugf("create new VirtualNetwork client failed with err %v", err)
		return nil, err
	}
	if d.IsRover {
		computeClient.Host = d.RoverComputeEndpoint
		vNetClient.Host = d.RoverNetworkEndpoint
		pool := x509.NewCertPool()
		pem, err := ioutil.ReadFile(d.RoverCertPath)
		if err != nil {
			panic("can not read cert " + err.Error())
		}
		pool.AppendCertsFromPEM(pem)
		if h, ok := computeClient.HTTPClient.(*http.Client); ok {
			tr := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}
			h.Transport = tr
		} else {
			panic("the client dispatcher is not of http.Client type. can not patch the tls config")
		}

		if h, ok := vNetClient.HTTPClient.(*http.Client); ok {
			//tr := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
			tr := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}
			h.Transport = tr
		} else {
			panic("the client dispatcher is not of http.Client type. can not patch the tls config")
		}
	}
	identityClient, err := identity.NewIdentityClientWithConfigurationProvider(configuration)
	if err != nil {
		log.Debugf("create new Identity client failed with err %v", err)
		return nil, err
	}
	c := &Client{
		configuration:        configuration,
		computeClient:        computeClient,
		virtualNetworkClient: vNetClient,
		identityClient:       identityClient,
		sleepDuration:        5,
	}
	return c, nil
}

// CreateInstance creates a new compute instance.
func (c *Client) CreateInstance(d *Driver, authorizedKeys string) (string, error) {
	displayName := defaultNodeNamePfx + d.MachineName
	availabilityDomain := d.AvailabilityDomain
	compartmentID := d.NodeCompartmentID
	nodeShape := d.Shape
	nodeImageName := d.Image
	nodeSubnetID := d.SubnetID
	var request core.LaunchInstanceRequest
	var err error
	if d.IsRover {
		log.Debug("inside rover")
		err, request = c.createReqForRover(displayName, availabilityDomain, compartmentID, nodeShape, nodeImageName, nodeSubnetID, authorizedKeys)
	} else {
		err, request = c.createReqForOCi(displayName, availabilityDomain, compartmentID, nodeShape, nodeImageName, nodeSubnetID, authorizedKeys)

	}
	if err != nil {
		return "", err
	}

	log.Debug("request is ", request)
	createResp, err := c.computeClient.LaunchInstance(context.Background(), request)
	if err != nil {
		return "", err
	}

	// wait until lifecycle status is Running
	pollUntilRunning := func(r common.OCIOperationResponse) bool {
		if converted, ok := r.Response.(core.GetInstanceResponse); ok {
			return converted.LifecycleState != core.InstanceLifecycleStateRunning
		}
		return true
	}

	// create get instance request with a retry policy which takes a function
	// to determine shouldRetry or not
	pollingGetRequest := core.GetInstanceRequest{
		InstanceId:      createResp.Instance.Id,
		RequestMetadata: helpers.GetRequestMetadataWithCustomizedRetryPolicy(pollUntilRunning),
	}

	instance, pollError := c.computeClient.GetInstance(context.Background(), pollingGetRequest)
	if pollError != nil {
		return "", err
	}

	return *instance.Id, nil
}

func (c *Client) createReqForOCi(displayName string, availabilityDomain string, compartmentID string, nodeShape string, nodeImageName string, nodeSubnetID string, authorizedKeys string) (error, core.LaunchInstanceRequest) {
	req := identity.ListAvailabilityDomainsRequest{}
	req.CompartmentId = &compartmentID
	ads, err := c.identityClient.ListAvailabilityDomains(context.Background(), req)
	if err != nil {
		return nil, core.LaunchInstanceRequest{}
	}

	// Just in case shortened or lower-case availability domain name was used
	log.Debugf("Resolving availability domain from %s", availabilityDomain)
	for _, ad := range ads.Items {
		if strings.Contains(*ad.Name, strings.ToUpper(availabilityDomain)) {
			log.Debugf("Availability domain %s", *ad.Name)
			availabilityDomain = *ad.Name
		}
	}

	imageID, err := c.getImageID(compartmentID, nodeImageName)
	if err != nil {
		return nil, core.LaunchInstanceRequest{}
	}
	// Create the launch compute instance request
	request := core.LaunchInstanceRequest{
		LaunchInstanceDetails: core.LaunchInstanceDetails{
			AvailabilityDomain: &availabilityDomain,
			CompartmentId:      &compartmentID,
			Shape:              &nodeShape,
			CreateVnicDetails: &core.CreateVnicDetails{
				SubnetId: &nodeSubnetID,
			},
			DisplayName: &displayName,
			Metadata: map[string]string{
				"ssh_authorized_keys": authorizedKeys,
				"user_data":           base64.StdEncoding.EncodeToString(createCloudInitScript()),
			},
			SourceDetails: core.InstanceSourceViaImageDetails{
				ImageId: imageID,
			},
		},
	}
	return err, request
}

func (c *Client) createReqForRover(displayName string, availabilityDomain string, compartmentID string, nodeShape string, nodeImageName string, nodeSubnetID string, authorizedKeys string) (error, core.LaunchInstanceRequest) {
	imageID, err := c.getImageID(compartmentID, nodeImageName)
	if err != nil {
		log.Error(err)
		log.Debug("inside error bhau", err)
		return nil, core.LaunchInstanceRequest{}
	}
	// Create the launch compute instance request
	request := core.LaunchInstanceRequest{
		LaunchInstanceDetails: core.LaunchInstanceDetails{
			AvailabilityDomain: common.String("OREI-1-AD-1"),
			CompartmentId:      &compartmentID,
			Shape:              &nodeShape,
			CreateVnicDetails: &core.CreateVnicDetails{
				SubnetId:       &nodeSubnetID,
				AssignPublicIp: common.Bool(true),
			},
			FaultDomain: common.String("FAULT-DOMAIN-1"),
			DisplayName: &displayName,
			Metadata: map[string]string{
				"ssh_authorized_keys": authorizedKeys,
				"user_data":           base64.StdEncoding.EncodeToString(createCloudInitScript()),
			},
			SourceDetails: core.InstanceSourceViaImageDetails{
				ImageId:             imageID,
				BootVolumeSizeInGBs: common.Int64(50),
			},
			AgentConfig: &core.LaunchInstanceAgentConfigDetails{
				IsMonitoringDisabled: common.Bool(true),
			},
		},
	}
	return err, request
}

// GetInstance gets a compute instance by id.
func (c *Client) GetInstance(id string) (core.Instance, error) {
	instanceResp, err := c.computeClient.GetInstance(context.Background(), core.GetInstanceRequest{InstanceId: &id})
	if err != nil {
		return core.Instance{}, err
	}
	return instanceResp.Instance, err
}

// TerminateInstance terminates a compute instance by id (does not wait).
func (c *Client) TerminateInstance(id string) error {
	_, err := c.computeClient.TerminateInstance(context.Background(), core.TerminateInstanceRequest{InstanceId: &id})
	return err
}

// StopInstance stops a compute instance by id and waits for it to reach the Stopped state.
func (c *Client) StopInstance(id string) error {

	actionRequest := core.InstanceActionRequest{}
	actionRequest.Action = core.InstanceActionActionStop
	actionRequest.InstanceId = &id

	stopResp, err := c.computeClient.InstanceAction(context.Background(), actionRequest)
	if err != nil {
		return err
	}

	// wait until lifecycle status is Stopped
	pollUntilStopped := func(r common.OCIOperationResponse) bool {
		if converted, ok := r.Response.(core.GetInstanceResponse); ok {
			return converted.LifecycleState != core.InstanceLifecycleStateStopped
		}
		return true
	}

	pollingGetRequest := core.GetInstanceRequest{
		InstanceId:      stopResp.Instance.Id,
		RequestMetadata: helpers.GetRequestMetadataWithCustomizedRetryPolicy(pollUntilStopped),
	}

	_, err = c.computeClient.GetInstance(context.Background(), pollingGetRequest)

	return err
}

// StartInstance starts a compute instance by id and waits for it to reach the Running state.
func (c *Client) StartInstance(id string) error {

	actionRequest := core.InstanceActionRequest{}
	actionRequest.Action = core.InstanceActionActionStart
	actionRequest.InstanceId = &id

	startResp, err := c.computeClient.InstanceAction(context.Background(), actionRequest)
	if err != nil {
		return err
	}

	// wait until lifecycle status is Running
	pollUntilRunning := func(r common.OCIOperationResponse) bool {
		if converted, ok := r.Response.(core.GetInstanceResponse); ok {
			return converted.LifecycleState != core.InstanceLifecycleStateRunning
		}
		return true
	}

	pollingGetRequest := core.GetInstanceRequest{
		InstanceId:      startResp.Instance.Id,
		RequestMetadata: helpers.GetRequestMetadataWithCustomizedRetryPolicy(pollUntilRunning),
	}

	_, err = c.computeClient.GetInstance(context.Background(), pollingGetRequest)

	return err
}

// RestartInstance stops and starts a compute instance by id and waits for it to be running again
func (c *Client) RestartInstance(id string) error {
	err := c.StopInstance(id)
	if err != nil {
		return err
	}
	return c.StartInstance(id)
}

// GetInstanceIP returns the public IP (or private IP if that is what it has).
func (c *Client) GetInstanceIP(id, compartmentID string) (string, error) {
	vnics, err := c.computeClient.ListVnicAttachments(context.Background(), core.ListVnicAttachmentsRequest{
		InstanceId:    &id,
		CompartmentId: &compartmentID,
	})
	if err != nil {
		return "", err
	}

	if len(vnics.Items) == 0 {
		return "", errors.New("instance does not have any configured VNICs")
	}

	vnic, err := c.virtualNetworkClient.GetVnic(context.Background(), core.GetVnicRequest{VnicId: vnics.Items[0].VnicId})
	if err != nil {
		return "", err
	}

	if vnic.PublicIp == nil {
		return *vnic.PrivateIp, nil
	}

	return *vnic.PublicIp, nil
}

// Create the cloud init script
func createCloudInitScript() []byte {
	cloudInit := []string{
		"#!/bin/sh",
		"#echo \"Disabling OS firewall...\"",
		"sudo /usr/sbin/ethtool --offload $(/usr/sbin/ip -o -4 route show to default | awk '{print $5}') tx off",
		"sudo iptables -F",
		"",
		"# Update to sellinux that fixes write permission error",
		"sudo yum install -y http://mirror.centos.org/centos/7/extras/x86_64/Packages/container-selinux-2.99-1.el7_6.noarch.rpm",
		"#sudo sed -i  s/SELINUX=enforcing/SELINUX=permissive/ /etc/selinux/config",
		"sudo setenforce 0",
		"sudo systemctl stop firewalld.service",
		"sudo systemctl disable firewalld.service",
		"",
		"echo \"Installing Docker...\"",
		"curl https://releases.rancher.com/install-docker/18.09.9.sh | sh",
		"sudo usermod -aG docker opc",
		"sudo systemctl enable docker",
		"",
		"# Elasticsearch requirement",
		"sudo sysctl -w vm.max_map_count=262144",
	}
	return []byte(strings.Join(cloudInit, "\n"))
}

// getImageID gets the most recent ImageId for the node image name
func (c *Client) getImageID(compartmentID, nodeImageName string) (*string, error) {

	if nodeImageName == "" || compartmentID == "" {
		return nil, errors.New("cannot retrieve image ID without a compartment and image name")
	}
	// Get list of images
	log.Debugf("Resolving image ID from %s", nodeImageName)
	var page *string
	for {
		request := core.ListImagesRequest{
			CompartmentId:  &compartmentID,
			SortBy:         core.ListImagesSortByTimecreated,
			SortOrder:      core.ListImagesSortOrderDesc,
			LifecycleState: core.ImageLifecycleStateAvailable,
			RequestMetadata: common.RequestMetadata{
				RetryPolicy: &common.RetryPolicy{
					MaximumNumberAttempts: 3,
					ShouldRetryOperation: func(r common.OCIOperationResponse) bool {
						return !(r.Error == nil && r.Response.HTTPResponse().StatusCode/100 == 2)
					},

					NextDuration: func(response common.OCIOperationResponse) time.Duration {
						return 3 * time.Second
					},
				},
			},
			Page: page,
		}
		//request := core.ListImagesRequest{CompartmentId: common.String(compartmentID)}
		r, err := c.computeClient.ListImages(context.Background(), request)
		log.Infof("r is", r)
		if err != nil {
			return nil, err
		}
		// Loop through the items to find an image to use.  The list is sorted by time created in descending order
		for _, image := range r.Items {
			if strings.EqualFold(*image.DisplayName, nodeImageName) {
				log.Infof("Provisioning node using image %s", *image.DisplayName)
				return image.Id, nil
			}
		}

		if page = r.OpcNextPage; r.OpcNextPage == nil {
			break
		}
	}

	return nil, fmt.Errorf("could not retrieve image id for an image named %s", nodeImageName)
}
