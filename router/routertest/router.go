// Copyright 2016 tsuru authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package routertest

import (
	"errors"
	"fmt"
	"net/url"
	"sync"

	"github.com/tsuru/tsuru/router"
)

var FakeRouter = newFakeRouter()

var HCRouter = hcRouter{fakeRouter: newFakeRouter()}

var ErrForcedFailure = errors.New("Forced failure")

func init() {
	router.Register("fake", createRouter)
	router.Register("fake-hc", createHCRouter)
}

func createRouter(name, prefix string) (router.Router, error) {
	return &FakeRouter, nil
}

func createHCRouter(name, prefix string) (router.Router, error) {
	return &HCRouter, nil
}

func newFakeRouter() fakeRouter {
	return fakeRouter{cnames: make(map[string]string), backends: make(map[string][]string), failuresByIp: make(map[string]bool), healthcheck: make(map[string]router.HealthcheckData), mutex: &sync.Mutex{}}
}

type fakeRouter struct {
	backends     map[string][]string
	cnames       map[string]string
	failuresByIp map[string]bool
	healthcheck  map[string]router.HealthcheckData
	mutex        *sync.Mutex
}

func (r *fakeRouter) FailForIp(ip string) {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	r.failuresByIp[ip] = true
}

func (r *fakeRouter) RemoveFailForIp(ip string) {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	delete(r.failuresByIp, ip)
}

func (r *fakeRouter) GetHealthcheck(name string) router.HealthcheckData {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	return r.healthcheck[name]
}

func (r *fakeRouter) HasBackend(name string) bool {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	_, ok := r.backends[name]
	return ok
}

func (r *fakeRouter) HasCName(name string) bool {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	_, ok := r.cnames[name]
	return ok
}

func (r *fakeRouter) HasRoute(name, address string) bool {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	routes, ok := r.backends[name]
	if !ok {
		routes, ok = r.backends[r.cnames[name]]
		if !ok {
			return false
		}
	}
	for _, route := range routes {
		if route == address {
			return true
		}
	}
	return false
}

func (r *fakeRouter) AddBackend(name string) error {
	if r.HasBackend(name) {
		return router.ErrBackendExists
	}
	r.mutex.Lock()
	defer r.mutex.Unlock()
	r.backends[name] = nil
	return router.Store(name, name, "fake")
}

func (r *fakeRouter) RemoveBackend(name string) error {
	backendName, err := router.Retrieve(name)
	if err != nil {
		return err
	}
	if backendName != name {
		return router.ErrBackendSwapped
	}
	if !r.HasBackend(backendName) {
		return router.ErrBackendNotFound
	}
	r.mutex.Lock()
	defer r.mutex.Unlock()
	for cname, backend := range r.cnames {
		if backend == backendName {
			delete(r.cnames, cname)
		}
	}
	delete(r.backends, backendName)
	return router.Remove(backendName)
}

func (r *fakeRouter) AddRoutes(name string, addresses []*url.URL) error {
	backendName, err := router.Retrieve(name)
	if err != nil {
		return err
	}
	if !r.HasBackend(backendName) {
		return router.ErrBackendNotFound
	}
	r.mutex.Lock()
	defer r.mutex.Unlock()
	for _, addr := range addresses {
		if r.failuresByIp[addr.String()] {
			return ErrForcedFailure
		}
	}
	routes := r.backends[backendName]
addresses:
	for _, addr := range addresses {
		for i := range routes {
			if routes[i] == addr.String() {
				continue addresses
			}
		}
		routes = append(routes, addr.String())
	}
	r.backends[backendName] = routes
	return nil
}

func (r *fakeRouter) RemoveRoutes(name string, addresses []*url.URL) error {
	backendName, err := router.Retrieve(name)
	if err != nil {
		return err
	}
	if !r.HasBackend(backendName) {
		return router.ErrBackendNotFound
	}
	r.mutex.Lock()
	defer r.mutex.Unlock()
	for _, addr := range addresses {
		if r.failuresByIp[addr.String()] {
			return ErrForcedFailure
		}
	}
	routes := r.backends[backendName]
	for _, addr := range addresses {
		for i := range routes {
			if routes[i] == addr.String() {
				routes = append(routes[:i], routes[i+1:]...)
				break
			}
		}
	}
	r.backends[backendName] = routes
	return nil
}

func (r *fakeRouter) AddRoute(name string, address *url.URL) error {
	backendName, err := router.Retrieve(name)
	if err != nil {
		return err
	}
	if !r.HasBackend(backendName) {
		return router.ErrBackendNotFound
	}
	if r.HasRoute(backendName, address.String()) {
		return router.ErrRouteExists
	}
	r.mutex.Lock()
	defer r.mutex.Unlock()
	if r.failuresByIp[address.String()] {
		return ErrForcedFailure
	}
	routes := r.backends[backendName]
	routes = append(routes, address.String())
	r.backends[backendName] = routes
	return nil
}

func (r *fakeRouter) RemoveRoute(name string, address *url.URL) error {
	backendName, err := router.Retrieve(name)
	if err != nil {
		return err
	}
	if !r.HasBackend(backendName) {
		return router.ErrBackendNotFound
	}
	r.mutex.Lock()
	defer r.mutex.Unlock()
	if r.failuresByIp[address.String()] {
		return ErrForcedFailure
	}
	index := -1
	routes := r.backends[backendName]
	for i := range routes {
		if routes[i] == address.String() {
			index = i
			break
		}
	}
	if index < 0 {
		return router.ErrRouteNotFound
	}
	routes[index] = routes[len(routes)-1]
	r.backends[backendName] = routes[:len(routes)-1]
	return nil
}

func (r *fakeRouter) SetCName(cname, name string) error {
	backendName, err := router.Retrieve(name)
	if err != nil {
		return err
	}
	if !r.HasBackend(backendName) {
		return nil
	}
	if !router.ValidCName(cname, "fakerouter.com") {
		return router.ErrCNameNotAllowed
	}
	r.mutex.Lock()
	defer r.mutex.Unlock()
	if _, ok := r.cnames[cname]; ok {
		return router.ErrCNameExists
	}
	r.cnames[cname] = backendName
	return nil
}

func (r *fakeRouter) UnsetCName(cname, name string) error {
	backendName, err := router.Retrieve(name)
	if err != nil {
		return err
	}
	if !r.HasBackend(backendName) {
		return nil
	}
	r.mutex.Lock()
	defer r.mutex.Unlock()
	if _, ok := r.cnames[cname]; !ok {
		return router.ErrCNameNotFound
	}
	delete(r.cnames, cname)
	return nil
}

func (r *fakeRouter) Addr(name string) (string, error) {
	backendName, err := router.Retrieve(name)
	if err != nil {
		return "", err
	}
	r.mutex.Lock()
	defer r.mutex.Unlock()
	if _, ok := r.backends[backendName]; ok {
		return fmt.Sprintf("%s.fakerouter.com", backendName), nil
	}
	return "", router.ErrBackendNotFound
}

func (r *fakeRouter) Reset() {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	r.backends = make(map[string][]string)
	r.failuresByIp = make(map[string]bool)
	r.cnames = make(map[string]string)
	r.healthcheck = make(map[string]router.HealthcheckData)
}

func (r *fakeRouter) Routes(name string) ([]*url.URL, error) {
	backendName, err := router.Retrieve(name)
	if err != nil {
		return nil, err
	}
	r.mutex.Lock()
	defer r.mutex.Unlock()
	routes := r.backends[backendName]
	result := make([]*url.URL, len(routes))
	for i, route := range routes {
		result[i], err = url.Parse(route)
		if err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (r *fakeRouter) Swap(backend1, backend2 string) error {
	return router.Swap(r, backend1, backend2)
}

type hcRouter struct {
	fakeRouter
	err error
}

func (r *hcRouter) SetErr(err error) {
	r.err = err
}

func (r *hcRouter) HealthCheck() error {
	return r.err
}

func (r *fakeRouter) SetHealthcheck(name string, data router.HealthcheckData) error {
	backendName, err := router.Retrieve(name)
	if err != nil {
		return err
	}
	r.mutex.Lock()
	defer r.mutex.Unlock()
	r.healthcheck[backendName] = data
	return nil
}
