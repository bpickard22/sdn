/*
Copyright 2016 The Kubernetes Authors.

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

package healthcheck

import (
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"

	"github.com/lithammer/dedent"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	api "k8s.io/kubernetes/pkg/apis/core"

	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	utilproxy "k8s.io/kubernetes/pkg/proxy/util"
)

// ServiceHealthServer serves HTTP endpoints for each service name, with results
// based on the endpoints.  If there are 0 endpoints for a service, it returns a
// 503 "Service Unavailable" error (telling LBs not to use this node).  If there
// are 1 or more endpoints, it returns a 200 "OK".
type ServiceHealthServer interface {
	// Make the new set of services be active.  Services that were open before
	// will be closed.  Services that are new will be opened.  Service that
	// existed and are in the new set will be left alone.  The value of the map
	// is the healthcheck-port to listen on.
	SyncServices(newServices map[types.NamespacedName]uint16) error
	// Make the new set of endpoints be active.  Endpoints for services that do
	// not exist will be dropped.  The value of the map is the number of
	// endpoints the service has on this node.
	SyncEndpoints(newEndpoints map[types.NamespacedName]int) error
}

type proxierHealthChecker interface {
	// IsHealthy returns the proxier's health state, following the same
	// definition the HTTP server defines.
	IsHealthy() bool
}

func newServiceHealthServer(hostname string, recorder events.EventRecorder, listener listener, factory httpServerFactory, nodePortAddresses *utilproxy.NodePortAddresses, healthzServer proxierHealthChecker) ServiceHealthServer {

	nodeAddresses, err := nodePortAddresses.GetNodeAddresses(utilproxy.RealNetwork{})
	if err != nil || nodeAddresses.Len() == 0 {
		klog.ErrorS(err, "Failed to get node ip address matching node port addresses, health check port will listen to all node addresses", "nodePortAddresses", nodePortAddresses)
		nodeAddresses = sets.NewString()
		nodeAddresses.Insert(utilproxy.IPv4ZeroCIDR)
	}

	// if any of the addresses is zero cidr then we listen
	// to old style :<port>
	for _, addr := range nodeAddresses.List() {
		if utilproxy.IsZeroCIDR(addr) {
			nodeAddresses = sets.NewString("")
			break
		}
	}

	return &server{
		hostname:      hostname,
		recorder:      recorder,
		listener:      listener,
		httpFactory:   factory,
		healthzServer: healthzServer,
		services:      map[types.NamespacedName]*hcInstance{},
		nodeAddresses: nodeAddresses,
	}
}

// NewServiceHealthServer allocates a new service healthcheck server manager
func NewServiceHealthServer(hostname string, recorder events.EventRecorder, nodePortAddresses *utilproxy.NodePortAddresses, healthzServer proxierHealthChecker) ServiceHealthServer {
	return newServiceHealthServer(hostname, recorder, stdNetListener{}, stdHTTPServerFactory{}, nodePortAddresses, healthzServer)
}

type server struct {
	hostname string
	// node addresses where health check port will listen on
	nodeAddresses sets.String
	recorder      events.EventRecorder // can be nil
	listener      listener
	httpFactory   httpServerFactory

	healthzServer proxierHealthChecker

	lock     sync.RWMutex
	services map[types.NamespacedName]*hcInstance
}

func (hcs *server) SyncServices(newServices map[types.NamespacedName]uint16) error {
	hcs.lock.Lock()
	defer hcs.lock.Unlock()

	// Remove any that are not needed any more.
	for nsn, svc := range hcs.services {
		if port, found := newServices[nsn]; !found || port != svc.port {
			klog.V(2).InfoS("Closing healthcheck", "service", nsn, "port", svc.port)

			// errors are loged in closeAll()
			_ = svc.closeAll()

			delete(hcs.services, nsn)

		}
	}

	// Add any that are needed.
	for nsn, port := range newServices {
		if hcs.services[nsn] != nil {
			klog.V(3).InfoS("Existing healthcheck", "service", nsn, "port", port)
			continue
		}

		klog.V(2).InfoS("Opening healthcheck", "service", nsn, "port", port)

		svc := &hcInstance{nsn: nsn, port: port}
		err := svc.listenAndServeAll(hcs)

		if err != nil {
			msg := fmt.Sprintf("node %s failed to start healthcheck %q on port %d: %v", hcs.hostname, nsn.String(), port, err)

			if hcs.recorder != nil {
				hcs.recorder.Eventf(
					&v1.ObjectReference{
						Kind:      "Service",
						Namespace: nsn.Namespace,
						Name:      nsn.Name,
						UID:       types.UID(nsn.String()),
					}, nil, api.EventTypeWarning, "FailedToStartServiceHealthcheck", "Listen", msg)
			}
			klog.ErrorS(err, "Failed to start healthcheck", "node", hcs.hostname, "service", nsn, "port", port)
			continue
		}
		hcs.services[nsn] = svc
	}
	return nil
}

type hcInstance struct {
	nsn  types.NamespacedName
	port uint16

	httpServers []httpServer

	endpoints int // number of local endpoints for a service
}

// listenAll opens health check port on all the addresses provided
func (hcI *hcInstance) listenAndServeAll(hcs *server) error {
	var err error
	var listener net.Listener

	addresses := hcs.nodeAddresses.List()
	hcI.httpServers = make([]httpServer, 0, len(addresses))

	// for each of the node addresses start listening and serving
	for _, address := range addresses {
		addr := net.JoinHostPort(address, fmt.Sprint(hcI.port))
		// create http server
		httpSrv := hcs.httpFactory.New(addr, hcHandler{name: hcI.nsn, hcs: hcs})
		// start listener
		listener, err = hcs.listener.Listen(addr)
		if err != nil {
			// must close whatever have been previously opened
			// to allow a retry/or port ownership change as needed
			_ = hcI.closeAll()
			return err
		}

		// start serving
		go func(hcI *hcInstance, listener net.Listener, httpSrv httpServer) {
			// Serve() will exit and return ErrServerClosed when the http server is closed.
			klog.V(3).InfoS("Starting goroutine for healthcheck", "service", hcI.nsn, "address", listener.Addr())
			if err := httpSrv.Serve(listener); err != nil && err != http.ErrServerClosed {
				klog.ErrorS(err, "Healthcheck closed", "service", hcI.nsn)
				return
			}
			klog.V(3).InfoS("Healthcheck closed", "service", hcI.nsn, "address", listener.Addr())
		}(hcI, listener, httpSrv)

		hcI.httpServers = append(hcI.httpServers, httpSrv)
	}

	return nil
}

func (hcI *hcInstance) closeAll() error {
	errors := []error{}
	for _, server := range hcI.httpServers {
		if err := server.Close(); err != nil {
			klog.ErrorS(err, "Error closing server for health check service", "service", hcI.nsn)
			errors = append(errors, err)
		}
	}

	if len(errors) > 0 {
		return utilerrors.NewAggregate(errors)
	}

	return nil
}

type hcHandler struct {
	name types.NamespacedName
	hcs  *server
}

var _ http.Handler = hcHandler{}

func (h hcHandler) ServeHTTP(resp http.ResponseWriter, req *http.Request) {
	h.hcs.lock.RLock()
	svc, ok := h.hcs.services[h.name]
	if !ok || svc == nil {
		h.hcs.lock.RUnlock()
		klog.ErrorS(nil, "Received request for closed healthcheck", "service", h.name)
		return
	}
	count := svc.endpoints
	kubeProxyHealthy := h.hcs.healthzServer.IsHealthy()
	h.hcs.lock.RUnlock()

	resp.Header().Set("Content-Type", "application/json")
	resp.Header().Set("X-Content-Type-Options", "nosniff")
	if count != 0 && kubeProxyHealthy {
		resp.WriteHeader(http.StatusOK)
	} else {
		resp.WriteHeader(http.StatusServiceUnavailable)
	}
	fmt.Fprint(resp, strings.Trim(dedent.Dedent(fmt.Sprintf(`
		{
			"service": {
				"namespace": %q,
				"name": %q
			},
			"localEndpoints": %d,
			"serviceProxyHealthy": %v
		}
		`, h.name.Namespace, h.name.Name, count, kubeProxyHealthy)), "\n"))
}

func (hcs *server) SyncEndpoints(newEndpoints map[types.NamespacedName]int) error {
	hcs.lock.Lock()
	defer hcs.lock.Unlock()

	for nsn, count := range newEndpoints {
		if hcs.services[nsn] == nil {
			continue
		}
		klog.V(3).InfoS("Reporting endpoints for healthcheck", "endpointCount", count, "service", nsn)
		hcs.services[nsn].endpoints = count
	}
	for nsn, hci := range hcs.services {
		if _, found := newEndpoints[nsn]; !found {
			hci.endpoints = 0
		}
	}
	return nil
}

// FakeServiceHealthServer is a fake ServiceHealthServer for test programs
type FakeServiceHealthServer struct{}

// NewFakeServiceHealthServer allocates a new fake service healthcheck server manager
func NewFakeServiceHealthServer() ServiceHealthServer {
	return FakeServiceHealthServer{}
}

// SyncServices is part of ServiceHealthServer
func (fake FakeServiceHealthServer) SyncServices(_ map[types.NamespacedName]uint16) error {
	return nil
}

// SyncEndpoints is part of ServiceHealthServer
func (fake FakeServiceHealthServer) SyncEndpoints(_ map[types.NamespacedName]int) error {
	return nil
}
