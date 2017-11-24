package proxy

import (
	"log"
	"net"
	"fmt"
	"net/url"
	"net/http"
	"net/http/httputil"
	"regexp"
	"os"
	"path/filepath"


	"github.com/docker/docker/runconfig"

	"io/ioutil"
	"bytes"
	"io"
)

// UpStream creates upstream handler struct
type UpStream struct {
	Name  string
	proxy http.Handler
	// TODO: Kick out separat config options and use more generic one
	allowed []*regexp.Regexp
	bindMounts []string
	devMappings 	[]string
	gpu 		bool
}

// UnixSocket just provides the path, so that I can test it
type UnixSocket struct {
	path string
}

// NewUnixSocket return a socket using the path
func NewUnixSocket(path string) UnixSocket {
	return UnixSocket{
		path: path,
	}
}

func (us *UnixSocket) connectSocket(proto, addr string) (net.Conn, error) {
	conn, err := net.Dial("unix", us.path)
	return conn, err
}

func newReverseProxy(dial func(network, addr string) (net.Conn, error)) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			param := ""
			if len(req.URL.RawQuery) > 0 {
				param = "?" + req.URL.RawQuery
			}
			u, _ := url.Parse("http://docker" + req.URL.Path + param)
			*req.URL = *u
		},
		Transport: &http.Transport{
			Dial: dial,
		},
	}
}

// NewUpstream returns a new socket (magic)
func NewUpstream(socket string, regs []string, binds []string, devs []string, gpu bool) *UpStream {
	us := NewUnixSocket(socket)
	a := []*regexp.Regexp{}
	for _, r := range regs {
		p, _ := regexp.Compile(r)
		a = append(a, p)
	}
	return &UpStream{
		Name:  socket,
		proxy: newReverseProxy(us.connectSocket),
		allowed: a,
		bindMounts: binds,
		devMappings: devs,
		gpu: gpu,
	}
}


func calculateContentLength(body io.Reader) (l int64, err error) {
	buf := &bytes.Buffer{}
	nRead, err := io.Copy(buf, body)
	if err != nil {
		fmt.Println(err)
	}
	l = nRead
	return
}

func (u *UpStream) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	/*if req.Method != "GET" {
		http.Error(w, fmt.Sprintf("Only GET requests are allowed, req.Method: %s", req.Method), 400)
		return
	}*/
	// Read the body
	body, err := ioutil.ReadAll(req.Body)
	if err != nil {
		fmt.Println(err.Error())
	}
	//fmt.Printf("%v\n", hostConfig.Mounts)
	// And now set a new body, which will simulate the same data we read:
	req.Body = ioutil.NopCloser(bytes.NewBuffer(body))
	if req.Body != nil && (req.ContentLength > 0 || req.ContentLength == -1) {
		// Decode the body
		dec := runconfig.ContainerDecoder{}
		config, hostConfig, networkingConfig, err := dec.DecodeConfig(req.Body)
		if err != nil {
			fmt.Printf("%s\n",err.Error())
		}
		// prepare devMappings
		devMappings := []string{}
		for _, dev := range u.devMappings {
			devMappings = append(devMappings, dev)
		}
		// In case GPU support is enabled add devices and mounts
		if u.gpu {
			fmt.Println("Add GPU stuff")
			// TODO: Be smarter about the version of the driver
			hostConfig.Binds = append(hostConfig.Binds, "/var/lib/nvidia-docker/volumes/nvidia_driver/384.81/:/usr/local/nvidia/")
			devMappings = append(devMappings, "/dev/nvidia0:/dev/nvidia0:rwm")
			devMappings = append(devMappings, "/dev/nvidia-uvm:/dev/nvidia-uvm:rwm")
			devMappings = append(devMappings, "/dev/nvidia-uvm-tools:/dev/nvidia-uvm-tools:rwm")
			devMappings = append(devMappings, "/dev/nvidiactl:/dev/nvidiactl:rwm")
			config.Env = append(config.Env, "PATH=/usr/local/nvidia/bin:/usr/local/cuda/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
		}
		for _, bMount := range u.bindMounts {
			if bMount == "" {
				continue
			}
			fmt.Printf("New bindmount: %s\n", bMount)
			hostConfig.Binds = append(hostConfig.Binds, bMount)
		}
		for _, dev := range devMappings {
			if dev == "" {
				continue
			}
			fmt.Printf("New device: %s\n", dev)

			dm, err := createDevMapping(dev)
			if err != nil {
				continue
			}
			hostConfig.Devices = append(hostConfig.Devices, dm)
		}
		fmt.Printf("Mounts: %v\n", hostConfig.Binds)
		cfgBody := configWrapper{
			Config:           config,
			HostConfig:       hostConfig,
			NetworkingConfig: networkingConfig,
		}
		nBody, _, err := encodeBody(cfgBody, req.Header)
		if err != nil {
			fmt.Printf("%s\n",err.Error())
		}
		req.Body = ioutil.NopCloser(nBody)
		nBody, _, _ = encodeBody(cfgBody, req.Header)
		newLength, _ := calculateContentLength(nBody)
		req.ContentLength = newLength
	} else {
		req.Body = ioutil.NopCloser(bytes.NewBuffer(body))
	}
	for _, a := range u.allowed {
		if a.MatchString(req.URL.Path) {
			u.proxy.ServeHTTP(w, req)
			return
		}
	}
	http.Error(w, fmt.Sprintf("'%s' is not allowed.", req.URL.Path), 403)
}

func ListenToNewSock(newsock string, sigc chan os.Signal) (l net.Listener, err error) {
	// extract directory for newsock
	dir, _ := filepath.Split(newsock)
	// attempt to create dir and ignore if it's already existing
	_ = os.Mkdir(dir, 0777)
	l, err = net.Listen("unix", newsock)
	if err != nil {
		panic(err)
	}
	os.Chmod(newsock, 0666)
	log.Println("[doxy] Listening on " + newsock)
	go func(c chan os.Signal) {
		sig := <-c
		log.Printf("[doxy] Caught signal %s: shutting down.\n", sig)
		if err := l.Close(); err != nil {
			panic(err)
		}
		os.Exit(0)
	}(sigc)
	return
}
