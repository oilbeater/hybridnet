/*
 Copyright 2021 The Hybridnet Authors.

 Licensed under the Apache License, Version 2.0 (the "License");
 you may not use this file except in compliance with the License.
 You may obtain a copy of the License at

     http://www.apache.org/licenses/LICENSE-2.0

 Unless required by applicable law or agreed to in writing, software
 distributed under the License is distributed on an "AS IS" BASIS,
 WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 See the License for the specific language governing permissions and
 limitations under the License.
*/

package server

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/go-logr/logr"

	corev1 "k8s.io/api/core/v1"

	"k8s.io/apimachinery/pkg/types"

	"sigs.k8s.io/controller-runtime/pkg/client"

	networkingv1 "github.com/alibaba/hybridnet/pkg/apis/networking/v1"
	"github.com/alibaba/hybridnet/pkg/constants"
	daemonconfig "github.com/alibaba/hybridnet/pkg/daemon/config"
	"github.com/alibaba/hybridnet/pkg/daemon/containernetwork"
	"github.com/alibaba/hybridnet/pkg/daemon/controller"
	"github.com/alibaba/hybridnet/pkg/request"

	"github.com/emicklei/go-restful"
)

type cniDaemonHandler struct {
	config       *daemonconfig.Configuration
	mgrClient    client.Client
	mgrAPIReader client.Reader

	logger logr.Logger
}

func createCniDaemonHandler(ctx context.Context, config *daemonconfig.Configuration,
	ctrlRef *controller.CtrlHub, logger logr.Logger) (*cniDaemonHandler, error) {
	cdh := &cniDaemonHandler{
		config:       config,
		mgrClient:    ctrlRef.GetMgrClient(),
		mgrAPIReader: ctrlRef.GetMgrAPIReader(),
		logger:       logger,
	}

	if ok := ctrlRef.CacheSynced(ctx); !ok {
		return nil, fmt.Errorf("failed to wait for ip instance & pod caches to sync")
	}

	return cdh, nil
}

func (cdh *cniDaemonHandler) handleAdd(req *restful.Request, resp *restful.Response) {
	podRequest := request.PodRequest{}
	err := req.ReadEntity(&podRequest)
	if err != nil {
		errMsg := fmt.Errorf("failed to parse add request: %v", err)
		cdh.errorWrapper(errMsg, http.StatusBadRequest, resp)
		return
	}
	cdh.logger.V(5).Info("handle add request", "content", podRequest)

	var macAddr string
	var netID *int32
	var affectedIPInstances []*networkingv1.IPInstance

	allocatedIPs := map[networkingv1.IPVersion]*containernetwork.IPInfo{
		networkingv1.IPv4: nil,
		networkingv1.IPv6: nil,
	}

	var returnIPAddress []request.IPAddress

	backOffBase := 5 * time.Microsecond
	retries := 11

	for i := 0; i < retries; i++ {
		time.Sleep(backOffBase)
		backOffBase = backOffBase * 2

		pod := &corev1.Pod{}
		if err := cdh.mgrAPIReader.Get(context.TODO(), types.NamespacedName{
			Name:      podRequest.PodName,
			Namespace: podRequest.PodNamespace,
		}, pod); err != nil {
			errMsg := fmt.Errorf("failed to get pod %v/%v: %v", podRequest.PodName, podRequest.PodNamespace, err)
			cdh.errorWrapper(errMsg, http.StatusBadRequest, resp)
			return
		}

		// wait for ip instance to be coupled
		annotation := pod.GetAnnotations()
		_, exist := annotation[constants.AnnotationIP]
		if exist {
			break
		} else if i == retries-1 {
			errMsg := fmt.Errorf("failed to wait for pod %v/%v be coupled with ip: %v", podRequest.PodName, podRequest.PodNamespace, err)
			cdh.errorWrapper(errMsg, http.StatusBadRequest, resp)
			return
		}
	}

	ipInstanceList := &networkingv1.IPInstanceList{}
	if err := cdh.mgrClient.List(context.TODO(), ipInstanceList, client.MatchingLabels{
		constants.LabelNode: cdh.config.NodeName,
		constants.LabelPod:  podRequest.PodName,
	}); err != nil {
		errMsg := fmt.Errorf("failed to list ip instance for pod %v: %v", cdh.config.NodeName, err)
		cdh.errorWrapper(errMsg, http.StatusBadRequest, resp)
		return
	}

	var networkName string
	for _, ipInstance := range ipInstanceList.Items {
		// IPv4 and IPv6 ip will exist at the same time
		if ipInstance.Status.PodName == podRequest.PodName && ipInstance.Status.PodNamespace == podRequest.PodNamespace {

			if netID == nil && macAddr == "" {
				netID = ipInstance.Spec.Address.NetID
				macAddr = ipInstance.Spec.Address.MAC
			} else if (netID != ipInstance.Spec.Address.NetID &&
				(netID != nil && *netID != *ipInstance.Spec.Address.NetID)) ||
				macAddr != ipInstance.Spec.Address.MAC {

				errMsg := fmt.Errorf("mac and netId for all ip instances of pod %v/%v should be the same", podRequest.PodNamespace, podRequest.PodName)
				cdh.errorWrapper(errMsg, http.StatusInternalServerError, resp)
				return
			}

			containerIP, cidrNet, err := net.ParseCIDR(ipInstance.Spec.Address.IP)
			if err != nil {
				errMsg := fmt.Errorf("failed to parse ip address %v to cidr: %v", ipInstance.Spec.Address.IP, err)
				cdh.errorWrapper(errMsg, http.StatusInternalServerError, resp)
				return
			}

			gatewayIP := net.ParseIP(ipInstance.Spec.Address.Gateway)

			ipVersion := networkingv1.IPv4
			switch ipInstance.Spec.Address.Version {
			case networkingv1.IPv4:
				if allocatedIPs[networkingv1.IPv4] != nil {
					errMsg := fmt.Errorf("only one ipv4 address for each pod are supported, %v/%v", podRequest.PodNamespace, podRequest.PodName)
					cdh.errorWrapper(errMsg, http.StatusInternalServerError, resp)
					return
				}

				allocatedIPs[networkingv1.IPv4] = &containernetwork.IPInfo{
					Addr: containerIP,
					Gw:   gatewayIP,
					Cidr: cidrNet,
				}
			case networkingv1.IPv6:
				if allocatedIPs[networkingv1.IPv6] != nil {
					errMsg := fmt.Errorf("only one ipv6 address for each pod are supported, %v/%v", podRequest.PodNamespace, podRequest.PodName)
					cdh.errorWrapper(errMsg, http.StatusInternalServerError, resp)
					return
				}

				allocatedIPs[networkingv1.IPv6] = &containernetwork.IPInfo{
					Addr: containerIP,
					Gw:   gatewayIP,
					Cidr: cidrNet,
				}

				ipVersion = networkingv1.IPv6
			default:
				errMsg := fmt.Errorf("unsupported ip version %v for pod %v/%v", ipInstance.Spec.Address.Version, podRequest.PodNamespace, podRequest.PodName)
				cdh.errorWrapper(errMsg, http.StatusInternalServerError, resp)
				return
			}

			currentNetworkName := ipInstance.Spec.Network
			if networkName == "" {
				networkName = currentNetworkName
			} else {
				if networkName != currentNetworkName {
					errMsg := fmt.Errorf("found different networks %v/%v for pod %v/%v", currentNetworkName, networkName, podRequest.PodNamespace, podRequest.PodName)
					cdh.errorWrapper(errMsg, http.StatusInternalServerError, resp)
					return
				}
			}

			returnIPAddress = append(returnIPAddress, request.IPAddress{
				IP:       ipInstance.Spec.Address.IP,
				Mac:      ipInstance.Spec.Address.MAC,
				Gateway:  ipInstance.Spec.Address.Gateway,
				Protocol: ipVersion,
			})

			affectedIPInstances = append(affectedIPInstances, &ipInstance)
		}
	}

	// check valid ip information second time
	if macAddr == "" || netID == nil {
		errMsg := fmt.Errorf("no available ip for pod %s/%s", podRequest.PodNamespace, podRequest.PodName)
		cdh.errorWrapper(errMsg, http.StatusInternalServerError, resp)
		return
	}

	network := &networkingv1.Network{}
	if err := cdh.mgrClient.Get(context.TODO(), types.NamespacedName{Name: networkName}, network); err != nil {
		errMsg := fmt.Errorf("cannot get network %v", networkName)
		cdh.errorWrapper(errMsg, http.StatusInternalServerError, resp)
		return
	}

	cdh.logger.Info("Create container",
		"podName", podRequest.PodName,
		"podNamespace", podRequest.PodNamespace,
		"ipAddr", printAllocatedIPs(allocatedIPs),
		"macAddr", macAddr,
		"netID", *netID)
	hostInterface, err := cdh.configureNic(podRequest.PodName, podRequest.PodNamespace, podRequest.NetNs, podRequest.ContainerID,
		macAddr, netID, allocatedIPs, networkingv1.GetNetworkMode(network))
	if err != nil {
		errMsg := fmt.Errorf("failed to configure nic: %v", err)
		cdh.errorWrapper(errMsg, http.StatusInternalServerError, resp)
		return
	}
	cdh.logger.Info("Container network created",
		"podName", podRequest.PodName,
		"podNamespace", podRequest.PodNamespace,
		"ipAddr", printAllocatedIPs(allocatedIPs),
		"macAddr", macAddr,
		"netID", *netID)

	// update IPInstance crd status
	for _, ip := range affectedIPInstances {
		newIPInstance := ip.DeepCopy()
		if newIPInstance == nil {
			errMsg := fmt.Errorf("failed to deepCopy IPInstance crd, no available for %s, %v", podRequest.PodName, err)
			cdh.errorWrapper(errMsg, http.StatusInternalServerError, resp)
			return
		}

		newIPInstance.Status.SandboxID = podRequest.ContainerID
		if err = cdh.mgrClient.Status().Update(context.TODO(), newIPInstance); err != nil {
			errMsg := fmt.Errorf("failed to update IPInstance crd for %s, %v", newIPInstance.Name, err)
			cdh.errorWrapper(errMsg, http.StatusInternalServerError, resp)
			return
		}
	}

	_ = resp.WriteHeaderAndEntity(http.StatusOK, request.PodResponse{
		IPAddress:     returnIPAddress,
		HostInterface: hostInterface,
	})
}

func (cdh *cniDaemonHandler) handleDel(req *restful.Request, resp *restful.Response) {
	podRequest := request.PodRequest{}
	err := req.ReadEntity(&podRequest)
	if err != nil {
		errMsg := fmt.Errorf("failed to parse del request: %v", err)
		cdh.errorWrapper(errMsg, http.StatusBadRequest, resp)
		return
	}
	cdh.logger.V(5).Info("handle del request", "content", podRequest)

	err = cdh.deleteNic(podRequest.NetNs)
	if err != nil {
		errMsg := fmt.Errorf("failed to del container nic for %s: %v",
			fmt.Sprintf("%s.%s", podRequest.PodName, podRequest.PodNamespace), err)
		cdh.errorWrapper(errMsg, http.StatusInternalServerError, resp)
		return
	}
	resp.WriteHeader(http.StatusNoContent)
}

func (cdh *cniDaemonHandler) errorWrapper(err error, status int, resp *restful.Response) {
	cdh.logger.Error(err, "handler error")
	_ = resp.WriteHeaderAndEntity(status, request.PodResponse{
		Err: err.Error(),
	})
}

func printAllocatedIPs(allocatedIPs map[networkingv1.IPVersion]*containernetwork.IPInfo) string {
	ipAddresseString := ""
	if allocatedIPs[networkingv1.IPv4] != nil && allocatedIPs[networkingv1.IPv4].Addr != nil {
		ipAddresseString = ipAddresseString + allocatedIPs[networkingv1.IPv4].Addr.String()
	}

	if allocatedIPs[networkingv1.IPv6] != nil && allocatedIPs[networkingv1.IPv6].Addr != nil {
		if ipAddresseString != "" {
			ipAddresseString = ipAddresseString + "/"
		}
		ipAddresseString = ipAddresseString + allocatedIPs[networkingv1.IPv6].Addr.String()
	}

	return ipAddresseString
}
