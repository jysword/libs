package services

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	etcdclient "github.com/coreos/etcd/client"
	log "github.com/gonet2/libs/nsq-logger"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
)

const (
	DEFAULT_ETCD         = "http://172.17.42.1:2379"
	DEFAULT_SERVICE_PATH = "/backends"
	DEFAULT_NAME_FILE    = "/backends/names"
	DEFAULT_DIAL_TIMEOUT = 10 * time.Second
	RETRY_DELAY          = 10 * time.Second
)

// a single connection
type client struct {
	key  string
	conn *grpc.ClientConn
}

// a kind of service
type service struct {
	clients []client
	idx     uint32 // for round-robin purpose
}

// all services
type service_pool struct {
	services          map[string]*service
	known_names       map[string]bool // store names.txt
	enable_name_check bool
	client            etcdclient.Client
	sync.RWMutex
}

var (
	_default_pool service_pool
	once          sync.Once
)

// Init() ***MUST*** be called before using
func Init() {
	once.Do(_default_pool.init)
}

func (p *service_pool) init() {
	// etcd client
	machines := []string{DEFAULT_ETCD}
	if env := os.Getenv("ETCD_HOST"); env != "" {
		machines = strings.Split(env, ";")
	}

	println(machines)
	// init etcd client
	cfg := etcdclient.Config{
		Endpoints: machines,
		Transport: etcdclient.DefaultTransport,
	}
	c, err := etcdclient.New(cfg)
	if err != nil {
		log.Critical(err)
		os.Exit(-1)
	}
	p.client = c

	// init
	p.services = make(map[string]*service)
	p.known_names = make(map[string]bool)
	p.load_names()
	p.connect_all(DEFAULT_SERVICE_PATH)
}

// get stored service name
func (p *service_pool) load_names() {
	kAPI := etcdclient.NewKeysAPI(p.client)
	// get the keys under directory
	log.Info("reading names:", DEFAULT_NAME_FILE)
	resp, err := kAPI.Get(context.Background(), DEFAULT_NAME_FILE, nil)
	if err != nil {
		log.Error(err)
		return
	}

	// validation check
	if resp.Node.Dir {
		log.Error("names is not a file")
		return
	}

	// split names
	names := strings.Split(resp.Node.Value, "\n")
	log.Info("all service names:", names)
	for _, v := range names {
		p.known_names[DEFAULT_SERVICE_PATH+"/"+strings.TrimSpace(v)] = true
	}

	p.enable_name_check = true
}

// connect to all services
func (p *service_pool) connect_all(directory string) {
	kAPI := etcdclient.NewKeysAPI(p.client)
	// get the keys under directory
	log.Info("connecting services under:", directory)
	resp, err := kAPI.Get(context.Background(), directory, &etcdclient.GetOptions{Recursive: true})
	if err != nil {
		log.Error(err)
		return
	}

	// validation check
	if !resp.Node.Dir {
		log.Error("not a directory")
		return
	}

	for _, node := range resp.Node.Nodes {
		if node.Dir { // service directory
			for _, service := range node.Nodes {
				p.add_service(service.Key, service.Value)
			}
		}
	}
	log.Info("services add complete")

	go p.watcher()
}

// watcher for data change in etcd directory
func (p *service_pool) watcher() {
	kAPI := etcdclient.NewKeysAPI(p.client)
	w := kAPI.Watcher(DEFAULT_SERVICE_PATH, &etcdclient.WatcherOptions{Recursive: true})
	for {
		resp, err := w.Next(context.Background())
		if err != nil {
			log.Error(err)
			continue
		}
		if resp.Node.Dir {
			continue
		}
		key, value := resp.Node.Key, resp.Node.Value
		if value == "" {
			log.Tracef("node delete: %v", key)
			p.remove_service(key)
		} else {
			log.Tracef("node add: %v %v", key, value)
			p.add_service(key, value)
		}
	}
}

// add a service
func (p *service_pool) add_service(key, value string) {
	p.Lock()
	defer p.Unlock()
	service_name := filepath.Dir(key)
	// name check
	if p.enable_name_check && !p.known_names[service_name] {
		log.Warningf("service not in names: %v, ignored", service_name)
		return
	}

	// try new service kind init
	if p.services[service_name] == nil {
		p.services[service_name] = &service{}
		log.Tracef("new service type: %v", service_name)
	}

	// create service connection
	service := p.services[service_name]
	if conn, err := grpc.Dial(value, grpc.WithTimeout(DEFAULT_DIAL_TIMEOUT), grpc.WithInsecure()); err == nil {
		service.clients = append(service.clients, client{key, conn})
		log.Tracef("service added: %v -- %v", key, value)
	} else {
		log.Errorf("did not connect: %v -- %v err: %v", key, value, err)
	}
}

// remove a service
func (p *service_pool) remove_service(key string) {
	p.Lock()
	defer p.Unlock()

	// check service kind
	service_name := filepath.Dir(key)
	service := p.services[service_name]
	if service == nil {
		log.Tracef("no such service %v", service_name)
		return
	}

	// remove a service
	for k := range service.clients {
		if service.clients[k].key == key { // deletion
			service.clients = append(service.clients[:k], service.clients[k+1:]...)
			log.Tracef("service removed %v", key)
			return
		}
	}
}

// provide a specific key for a service, eg:
// path:/backends/snowflake, id:s1
//
// the full cannonical path for this service is:
// 			/backends/snowflake/s1
func (p *service_pool) get_service_with_id(path string, id string) *grpc.ClientConn {
	p.RLock()
	defer p.RUnlock()
	// check existence
	service := p.services[path]
	if service == nil {
		return nil
	}
	if len(service.clients) == 0 {
		return nil
	}

	// loop find a service with id
	fullpath := string(path) + "/" + id
	for k := range service.clients {
		if service.clients[k].key == fullpath {
			return service.clients[k].conn
		}
	}

	return nil
}

// get a service in round-robin style
// especially useful for load-balance with state-less services
func (p *service_pool) get_service(path string) *grpc.ClientConn {
	p.RLock()
	defer p.RUnlock()
	// check existence
	service := p.services[path]
	if service == nil {
		return nil
	}

	if len(service.clients) == 0 {
		return nil
	}

	// get a service in round-robind style,
	idx := int(atomic.AddUint32(&service.idx, 1))
	return service.clients[idx%len(service.clients)].conn
}

/////////////////////////////////////////////////////////////////
// Wrappers
func GetService(path string) *grpc.ClientConn {
	return _default_pool.get_service(path)
}

func GetServiceWithId(path string, id string) *grpc.ClientConn {
	return _default_pool.get_service_with_id(path, id)
}
