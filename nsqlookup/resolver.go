package nsqlookup

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Resolver is an interface implemented by types that provide a list of server
// addresses.
type Resolver interface {
	Resolve(ctx context.Context) ([]string, error)
}

// ResolverFunc makes it possible to use regular function types as resolvers.
type ResolverFunc func(ctx context.Context) ([]string, error)

// Resolve satisfies the Resolver interface.
func (f ResolverFunc) Resolve(ctx context.Context) ([]string, error) {
	return f(ctx)
}

// Servers is the implementation of a Resolver that always returns the same list
// of servers.
type Servers []string

// Resolve satisfies the Resolver interface.
func (r Servers) Resolve(ctx context.Context) ([]string, error) {
	if ctx != nil {
		select {
		default:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return r.copy(), nil
}

func (r Servers) copy() []string {
	if len(r) == 0 {
		return nil
	}
	s := make([]string, len(r))
	copy(s, r)
	return s
}

// CachedResolver implements a time-based cache that wraps a resolver.
type CachedResolver struct {
	Resolver Resolver
	Timeout  time.Duration

	mutex   sync.RWMutex
	exptime time.Time
	servers Servers
	error   error
}

// Resolve statisfies the Resolver interface.
func (r *CachedResolver) Resolve(ctx context.Context) (res []string, err error) {
	var now = time.Now()
	var ok bool

	if res, err, ok = r.get(now); ok {
		return
	}

	defer r.mutex.Unlock()
	r.mutex.Lock()

	if now.Before(r.exptime) {
		res, err = r.servers.copy(), r.error
		return
	}

	if res, err = r.Resolver.Resolve(ctx); err == context.Canceled {
		r.servers = nil
		r.error = nil
		r.exptime = time.Time{}
		return
	}

	r.servers = Servers(res)
	r.error = err
	r.exptime = now.Add(r.Timeout)
	return
}

func (r *CachedResolver) get(now time.Time) (res []string, err error, ok bool) {
	r.mutex.RLock()

	if now.Before(r.exptime) {
		res, err, ok = r.servers.copy(), r.error, true
	}

	r.mutex.RUnlock()
	return
}

// ConsulResolver implements a resolver which discovery nsqlookupd servers from
// a consul catalog.
type ConsulResolver struct {
	Address   string
	Service   string
	Transport http.RoundTripper
}

func (r *ConsulResolver) Resolve(ctx context.Context) (list []string, err error) {
	var address = r.Address
	var service = r.Service
	var t http.RoundTripper

	if t = r.Transport; t == nil {
		t = http.DefaultTransport
	}

	if len(address) == 0 {
		address = "http://localhost:8500"
	}

	if len(service) == 0 {
		service = "nsqlookupd"
	}

	if strings.Index(address, "://") < 0 {
		address = "http://" + address
	}

	// get list of check results for service
	var checksResults []struct {
		Node string
	}

	checksBody, _ := r.getConsul(ctx, fmt.Sprintf("v1/health/checks/%s?passing", service))
	if err = json.Unmarshal(checksBody, &checksResults); err != nil {
		return
	}

	// get list of nodes for service
	var serviceResults []struct {
		Node           string
		Address        string
		ServiceAddress string
		ServicePort    int
	}

	serviceBody, _ := r.getConsul(ctx, fmt.Sprintf("v1/catalog/service/%s", service))
	if err = json.Unmarshal(serviceBody, &serviceResults); err != nil {
		return
	}

	list = make([]string, 0, len(checksResults))

	for _, r := range serviceResults {
		var passing bool
		for _, c := range checksResults {
			if c.Node == r.Node {
				passing = true
				break
			}
		}

		if passing {
			host := r.ServiceAddress
			port := r.ServicePort

			if len(host) == 0 {
				host = r.Address
			}

			list = append(list, net.JoinHostPort(host, strconv.Itoa(port)))
		}
	}

	return
}

func (r *ConsulResolver) getConsul(ctx context.Context, endpoint string) ([]byte, error) {
	var address = r.Address
	var req *http.Request
	var res *http.Response
	var t http.RoundTripper
	var err error

	if t = r.Transport; t == nil {
		t = http.DefaultTransport
	}

	if len(address) == 0 {
		address = "http://localhost:8500"
	}

	if strings.Index(address, "://") < 0 {
		address = "http://" + address
	}

	// Get list of check results for service
	if req, err = http.NewRequest("GET", fmt.Sprintf("%s/%s", address, endpoint), nil); err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "nsqlookup consul resolver")
	req.Header.Set("Accept", "application/json")

	if ctx != nil {
		req = req.WithContext(ctx)
	}

	if res, err = t.RoundTrip(req); err != nil {
		return nil, err
	}
	defer res.Body.Close()

	switch res.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, err
	default:
		err = fmt.Errorf("error looking up %s on consul agent at %s: %d %s", endpoint, address, res.StatusCode, res.Status)
		return nil, err
	}

	body, _ := ioutil.ReadAll(res.Body)
	return body, nil
}

// MultiResolver returns a resolver that merges all resolves from rslv when its
// own Resolve method is called.
func MultiResolver(rslv ...Resolver) Resolver {
	list := make([]Resolver, len(rslv))
	copy(list, rslv)
	return &multiResolver{list}
}

type multiResolver struct {
	list []Resolver
}

func (m *multiResolver) Resolve(ctx context.Context) (res []string, err error) {
	if len(m.list) == 0 {
		return nil, nil
	}

	if len(m.list) == 1 {
		return m.list[0].Resolve(ctx)
	}

	type result struct {
		res []string
		err error
	}

	reschan := make(chan result, len(m.list))

	for _, rslv := range m.list {
		go func(rslv Resolver) {
			res, err := rslv.Resolve(ctx)
			reschan <- result{
				res: res,
				err: err,
			}
		}(rslv)
	}

	for i, n := 0, len(m.list); i != n; i++ {
		if r := <-reschan; r.err != nil {
			err = appendError(err, r.err)
		} else {
			res = append(res, r.res...)
		}
	}

	if len(res) != 0 {
		err = nil
	}

	return
}
