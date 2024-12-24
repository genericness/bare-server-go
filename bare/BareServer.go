package bare

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

const (
	maxHeaderValue = 3072
)

type BareError struct {
	Status int `json:"-"`
	// Status  int    `json:"status"`
	Code    string `json:"code"`
	ID      string `json:"id"`
	Message string `json:"message,omitempty"`
	Stack   string `json:"stack,omitempty"`
}

type BareServer struct {
	directory    string
	routes       map[string]RouteCallback
	socketRoutes map[string]SocketRouteCallback
	versions     []string
	closed       bool
	options      *Options
	wss          *websocket.Upgrader
}

type Options struct {
	LogErrors    bool
	FilterRemote func(*url.URL) *BareError
	Lookup       func(hostname string, service string, hints ...net.IPAddr) (addrs []net.IPAddr, err error)
	LocalAddress string
	Family       int
	Maintainer   *BareMaintainer
	httpAgent    *http.Transport
	httpsAgent   *http.Transport
}

type RouteCallback func(request *BareRequest, response http.ResponseWriter, options *Options) *BareError

type SocketRouteCallback func(request *BareRequest, conn *websocket.Conn, options *Options) error

type BareRequest struct {
	*http.Request
	Native *http.Request
}

type BareMaintainer struct {
	Email   string `json:"email,omitempty"`
	Website string `json:"website,omitempty"`
}

type BareProject struct {
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	Email       string `json:"email,omitempty"`
	Website     string `json:"website,omitempty"`
	Repository  string `json:"repository,omitempty"`
	Version     string `json:"version,omitempty"`
}

type BareLanguage string

type BareManifest struct {
	Versions    []string        `json:"versions"`
	Language    BareLanguage    `json:"language"`
	MemoryUsage float64         `json:"memoryUsage,omitempty"`
	Project     *BareProject    `json:"project,omitempty"`
	Maintainer  *BareMaintainer `json:"maintainer,omitempty"`
}

func CreateBareServer(directory string, options *Options) *BareServer {
	if options.FilterRemote == nil {
		options.FilterRemote = func(remote *url.URL) *BareError {
			if isValidIP(remote.Hostname()) && !parseIP(remote.Hostname()).IsGlobalUnicast() {
				return &BareError{403, "Forbidden", "UNKNOWN", "forbidden IP", ""}
			}
			return nil
		}
	}

	if options.Lookup == nil {
		options.Lookup = func(hostname string, service string, hints ...net.IPAddr) (addrs []net.IPAddr, err error) {
			ips, err := net.LookupIP(hostname)
			if err != nil {
				return nil, err
			}
			for _, ip := range ips {
				addrs = append(addrs, net.IPAddr{IP: ip})
			}
			return addrs, nil
		}
	}

	if options.httpAgent == nil {
		options.httpAgent = &http.Transport{
			DialContext: (&net.Dialer{
				LocalAddr: getLocalAddr(options.LocalAddress, options.Family),
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
				DualStack: true,
			}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		}
	}

	options.httpAgent.DisableCompression = true

	if options.httpsAgent == nil {
		options.httpsAgent = &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
			},
			DialContext: (&net.Dialer{
				LocalAddr: getLocalAddr(options.LocalAddress, options.Family),
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
				DualStack: true,
			}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		}
	}

	options.httpsAgent.DisableCompression = true

	server := &BareServer{
		directory:    directory,
		routes:       make(map[string]RouteCallback),
		socketRoutes: make(map[string]SocketRouteCallback),
		versions:     make([]string, 0),
		options:      options,
		wss: &websocket.Upgrader{
			ReadBufferSize:  1024,
			WriteBufferSize: 1024,
			CheckOrigin: func(r *http.Request) bool {
				return true
			},
		},
	}

	registerV3(server)

	return server
}

func (s *BareServer) Close() {
	s.closed = true
}

func (s *BareServer) ShouldRoute(request *http.Request) bool {
	return !s.closed && strings.HasPrefix(request.URL.Path, s.directory)
}

func (s *BareServer) RouteUpgrade(w http.ResponseWriter, r *http.Request, conn *websocket.Conn) {
	request := &BareRequest{
		Request: r,
		Native:  r,
	}

	service := r.URL.Path[len(s.directory)-1:]

	if handler, ok := s.socketRoutes[service]; ok {
		if err := handler(request, conn, s.options); err != nil {
			if s.options.LogErrors {
				fmt.Fprintf(os.Stderr, "Error in socket handler: %s\n", err)
			}
			conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseInternalServerErr, err.Error()), time.Now().Add(time.Second*10))
			conn.Close()
			return
		}
	} else {
		conn.Close()
	}
}

func (s *BareServer) RouteRequest(w http.ResponseWriter, r *http.Request) {
	request := &BareRequest{
		Request: r,
		Native:  r,
	}

	service := r.URL.Path[len(s.directory)-1:]

	var err *BareError

	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusOK)
	} else if service == "/" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		enc := json.NewEncoder(w)
		enc.SetIndent("", "\t")
		enc.Encode(s.getInstanceInfo())
	} else if handler, ok := s.routes[service]; ok {
		err = handler(request, w, s.options)
		if s.options.LogErrors && err != nil {
			fmt.Fprintf(os.Stderr, "%s\n", err.Message)
		}
	} else {
		err = &BareError{404, "UNKNOWN", "error.NotFoundError", "Not Found", ""}
	}

	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(err.Status)
		enc := json.NewEncoder(w)
		enc.SetIndent("", "\t")
		enc.Encode(*err)
	}
}

func (s *BareServer) getInstanceInfo() BareManifest {
	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)

	info := BareManifest{
		Versions:    s.versions,
		Language:    "Go",
		MemoryUsage: float64(memStats.HeapAlloc) / 1024 / 1024,
		Maintainer:  s.options.Maintainer,
		Project: &BareProject{
			Name:        "bare-server-go",
			Description: "Bare server implementation in Go",
			Repository:  "https://github.com/tomphttp/bare-server-go",
			Version:     "0.1.0",
		},
	}

	return info
}

func (s *BareServer) Handle(pattern string, handler RouteCallback) {
	s.routes[pattern] = handler
}

func (s *BareServer) HandleSocket(pattern string, handler SocketRouteCallback) {
	s.socketRoutes[pattern] = handler
}

func (s *BareServer) Start(addr string) error {
	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if s.ShouldRoute(r) {
			addCors(w)
			if websocket.IsWebSocketUpgrade(r) {
				conn, err := s.wss.Upgrade(w, r, nil)
				if err != nil {
					fmt.Fprintf(os.Stderr, "Error upgrading to websocket: %s\n", err)
					return
				}
				s.RouteUpgrade(w, r, conn)
			} else {
				s.RouteRequest(w, r)
			}
		} else {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, "Not found")
		}
	})

	server := &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "Error starting server: %s\n", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)
	<-stop

	fmt.Println("Shutting down server...")
	if err := server.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "Error shutting down server: %s\n", err)
	}

	return nil
}

func splitHeaders(headers http.Header) http.Header {
	output := make(http.Header)
	for key, values := range headers {
		output[key] = values
	}

	if values, ok := headers["X-Bare-Headers"]; ok {
		value := strings.Join(values, ", ")
		if len(value) > maxHeaderValue {
			delete(output, "X-Bare-Headers")
			split := 0
			for i := 0; i < len(value); i += maxHeaderValue {
				part := value[i:min(i+maxHeaderValue, len(value))]
				id := strconv.Itoa(split)
				output.Add(fmt.Sprintf("X-Bare-Headers-%s", id), ";"+part)
				split++
			}
		}
	}

	return output
}

func joinHeaders(headers http.Header) http.Header {
	output := make(http.Header)
	for key, values := range headers {
		output[key] = values
	}

	prefix := "x-bare-headers-"
	if _, ok := headers[prefix+"0"]; ok {
		var join []string
		for header := range headers {
			if strings.HasPrefix(strings.ToLower(header), prefix) {
				value := headers.Get(header)
				if !strings.HasPrefix(value, ";") {
					panic(&BareError{400, "INVALID_BARE_HEADER", fmt.Sprintf("request.headers.%s", header), "Value didn't begin with semi-colon.", ""})
				}
				join = append(join, value[1:])
				delete(output, header)
			}
		}
		output.Set("x-bare-headers", strings.Join(join, ""))
	}

	return output
}

func outgoingError(err error) *BareError {
	if netErr, ok := err.(net.Error); ok {
		if netErr.Timeout() {
			return &BareError{500, "CONNECTION_TIMEOUT", "response", "The response timed out.", ""}
		}
		if opErr, ok := netErr.(*net.OpError); ok {
			switch opErr.Err.Error() {
			case "no such host":
				return &BareError{500, "HOST_NOT_FOUND", "request", "The specified host could not be resolved.", ""}
			case "connection refused":
				return &BareError{500, "CONNECTION_REFUSED", "response", "The remote rejected the request.", ""}
			case "connection reset by peer":
				return &BareError{500, "CONNECTION_RESET", "response", "The request was forcibly closed.", ""}
			}
		}
	}

	return &BareError{500, "UNKNOWN", "unknown", "", ""}
	//return errerr
}

func bareFetch(request *BareRequest, requestHeaders http.Header, remote *url.URL, options *Options) (*http.Response, *BareError) {
	if options.FilterRemote != nil {
		if err := options.FilterRemote(remote); err != nil {
			return nil, err
		}
	}

	req, err := http.NewRequest(request.Method, remote.String(), request.Body)
	if err != nil {
		return nil, outgoingError(err)
	}

	req.Header = requestHeaders

	var client *http.Client
	if remote.Scheme == "https" {
		if options.httpsAgent != nil {
			client = &http.Client{Transport: options.httpsAgent}
		} else {
			client = &http.Client{}
		}
	} else {
		if options.httpAgent != nil {
			client = &http.Client{Transport: options.httpAgent}
		} else {
			client = &http.Client{}
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, outgoingError(err)
	}

	return resp, nil
}

func webSocketFetch(requestHeaders http.Header, remote *url.URL, protocols []string, options *Options) (*http.Response, *websocket.Conn, *http.Request, *BareError) {
	if options.FilterRemote != nil {
		if err := options.FilterRemote(remote); err != nil {
			return nil, nil, nil, err
		}
	}

	dialer := &websocket.Dialer{
		Proxy:            http.ProxyFromEnvironment,
		HandshakeTimeout: 12 * time.Second,
		NetDialContext:   (&net.Dialer{LocalAddr: getLocalAddr(options.LocalAddress, options.Family)}).DialContext,
		Subprotocols:     protocols,
	}

	conn, resp, err := dialer.Dial(remote.String(), requestHeaders)
	if err != nil {
		return nil, nil, nil, outgoingError(err)
	}

	return resp, conn, resp.Request, nil
}

func addCors(w http.ResponseWriter) {
	w.Header().Set("x-robots-tag", "noindex")
	w.Header().Set("access-control-allow-headers", "*")
	w.Header().Set("access-control-allow-origin", "*")
	w.Header().Set("access-control-allow-methods", "*")
	w.Header().Set("access-control-expose-headers", "*")
	w.Header().Set("access-control-max-age", "7200")
}

func isValidIP(ip string) bool {
	return net.ParseIP(ip) != nil
}

func parseIP(ip string) net.IP {
	return net.ParseIP(ip)
}

func getLocalAddr(localAddress string, family int) net.Addr {
	if localAddress != "" {
		if ip := net.ParseIP(localAddress); ip != nil {
			if family == 0 || ip.To4() != nil && family == 4 || ip.To16() != nil && family == 6 {
				return &net.TCPAddr{IP: ip}
			}
		}
	}
	return nil
}
