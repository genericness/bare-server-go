// SPDX-License-Identifier: MPL-2.0
// tomphttp/bare-server-go
package main

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/spf13/cobra"
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

const (
	LanguageNodeJS        BareLanguage = "NodeJS"
	LanguageServiceWorker BareLanguage = "ServiceWorker"
	LanguageDeno          BareLanguage = "Deno"
	LanguageJava          BareLanguage = "Java"
	LanguagePHP           BareLanguage = "PHP"
	LanguageRust          BareLanguage = "Rust"
	LanguageC             BareLanguage = "C"
	LanguageCPlusPlus     BareLanguage = "C++"
	LanguageCSharp        BareLanguage = "C#"
	LanguageRuby          BareLanguage = "Ruby"
	LanguageGo            BareLanguage = "Go"
	LanguageCrystal       BareLanguage = "Crystal"
	LanguageShell         BareLanguage = "Shell"
)

type BareManifest struct {
	Maintainer  *BareMaintainer `json:"maintainer,omitempty"`
	Project     *BareProject    `json:"project,omitempty"`
	Versions    []string        `json:"versions"`
	Language    BareLanguage    `json:"language"`
	MemoryUsage float64         `json:"memoryUsage,omitempty"`
}

func NewBareServer(directory string, options *Options) *BareServer {
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
		Language:    LanguageGo,
		MemoryUsage: float64(memStats.HeapAlloc) / 1024 / 1024,
		Maintainer:  s.options.Maintainer,
		Project: &BareProject{
			Name:        "bare-server-go",
			Description: "Bare server implementation in Go",
			Repository:  "https://github.com/genericness/bare-server-go",
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

func JSON(w http.ResponseWriter, statusCode int, data interface{}) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	return json.NewEncoder(w).Encode(data)
}

type SocketClientToServer struct {
	Type           string            `json:"type"`
	Remote         string            `json:"remote"`
	Protocols      []string          `json:"protocols"`
	Headers        map[string]string `json:"headers"`
	ForwardHeaders []string          `json:"forwardHeaders"`
}

type SocketServerToClient struct {
	Type       string   `json:"type"`
	Protocol   string   `json:"protocol"`
	SetCookies []string `json:"setCookies"`
}

func addCors(w http.ResponseWriter) {
	w.Header().Set("x-robots-tag", "noindex")
	w.Header().Set("access-control-allow-headers", "*")
	w.Header().Set("access-control-allow-origin", "*")
	w.Header().Set("access-control-allow-methods", "*")
	w.Header().Set("access-control-expose-headers", "*")
	w.Header().Set("access-control-max-age", "7200")
}

func registerV3(server *BareServer) {
	forbiddenSendHeaders := []string{"connection", "content-length", "transfer-encoding"}
	forbiddenForwardHeaders := []string{"connection", "transfer-encoding", "host", "origin", "referer"}
	forbiddenPassHeaders := []string{"vary", "connection", "transfer-encoding", "access-control-allow-headers", "access-control-allow-methods", "access-control-expose-headers", "access-control-max-age", "access-control-request-headers", "access-control-request-method"}
	defaultForwardHeaders := []string{"accept-encoding", "accept-language"}
	defaultPassHeaders := []string{"content-encoding", "content-length", "last-modified"}
	defaultCacheForwardHeaders := []string{"if-modified-since", "if-none-match", "cache-control"}
	defaultCachePassHeaders := []string{"cache-control", "etag"}
	cacheNotModified := 304

	loadForwardedHeaders := func(forward []string, target http.Header, request *BareRequest) {
		for _, header := range forward {
			if value := request.Header.Get(header); value != "" {
				target.Set(header, value)
			}
		}
	}

	splitHeaderValue := regexp.MustCompile(`,\s*`)

	readHeaders := func(request *BareRequest) (map[string]interface{}, *BareError) {
		sendHeaders := make(http.Header)
		passHeaders := append([]string{}, defaultPassHeaders...)
		var passStatus []int
		forwardHeaders := append([]string{}, defaultForwardHeaders...)

		cache, _ := strconv.ParseBool(request.URL.Query().Get("cache"))

		if cache {
			passHeaders = append(passHeaders, defaultCachePassHeaders...)
			passStatus = append(passStatus, cacheNotModified)
			forwardHeaders = append(forwardHeaders, defaultCacheForwardHeaders...)
		}

		headers := joinHeaders(request.Header)

		xBareURL := headers.Get("x-bare-url")
		if xBareURL == "" {
			return nil, &BareError{400, "MISSING_BARE_HEADER", "request.headers.x-bare-url", "Header was not specified.", ""}
		}

		remote, err := url.Parse(xBareURL)
		if err != nil {
			return nil, &BareError{400, "INVALID_BARE_HEADER", "request.headers.x-bare-url", "Invalid URL.", ""}
		}

		xBareHeaders := headers.Get("x-bare-headers")
		if xBareHeaders == "" {
			return nil, &BareError{400, "MISSING_BARE_HEADER", "request.headers.x-bare-headers", "Header was not specified.", ""}
		}

		var jsonHeaders map[string]interface{}
		if err := json.Unmarshal([]byte(xBareHeaders), &jsonHeaders); err != nil {
			return nil, &BareError{400, "INVALID_BARE_HEADER", "request.headers.x-bare-headers", "Header contained invalid JSON.", ""}
		}

		for header, value := range jsonHeaders {
			if contains(forbiddenSendHeaders, strings.ToLower(header)) {
				continue
			}
			switch v := value.(type) {
			case string:
				sendHeaders.Set(header, v)
			case []interface{}:
				for _, v := range v {
					if strVal, ok := v.(string); ok {
						sendHeaders.Add(header, strVal)
					} else {
						return nil, &BareError{400, "INVALID_BARE_HEADER", fmt.Sprintf("bare.headers.%s", header), "Header value must be a string or an array of strings.", ""}
					}
				}
			default:
				return nil, &BareError{400, "INVALID_BARE_HEADER", fmt.Sprintf("bare.headers.%s", header), "Header value must be a string or an array of strings.", ""}
			}
		}

		if xBarePassStatus := headers.Get("x-bare-pass-status"); xBarePassStatus != "" {
			for _, value := range splitHeaderValue.Split(xBarePassStatus, -1) {
				number, err := strconv.Atoi(value)
				if err != nil {
					return nil, &BareError{400, "INVALID_BARE_HEADER", "request.headers.x-bare-pass-status", "Array contained non-number value.", ""}
				}
				passStatus = append(passStatus, number)
			}
		}

		if xBarePassHeaders := headers.Get("x-bare-pass-headers"); xBarePassHeaders != "" {
			for _, header := range splitHeaderValue.Split(xBarePassHeaders, -1) {
				header = strings.ToLower(header)
				if contains(forbiddenPassHeaders, header) {
					return nil, &BareError{400, "FORBIDDEN_BARE_HEADER", "request.headers.x-bare-forward-headers", "A forbidden header was passed.", ""}
				}
				passHeaders = append(passHeaders, header)
			}
		}

		if xBareForwardHeaders := headers.Get("x-bare-forward-headers"); xBareForwardHeaders != "" {
			for _, header := range splitHeaderValue.Split(xBareForwardHeaders, -1) {
				header = strings.ToLower(header)
				if contains(forbiddenForwardHeaders, header) {
					return nil, &BareError{400, "FORBIDDEN_BARE_HEADER", "request.headers.x-bare-forward-headers", "A forbidden header was forwarded.", ""}
				}
				forwardHeaders = append(forwardHeaders, header)
			}
		}

		result := map[string]interface{}{
			"remote":         remote,
			"sendHeaders":    sendHeaders,
			"passHeaders":    passHeaders,
			"passStatus":     passStatus,
			"forwardHeaders": forwardHeaders,
		}

		return result, nil
	}

	server.Handle("/v3/", func(request *BareRequest, w http.ResponseWriter, options *Options) *BareError {
		headersData, err := readHeaders(request)
		if err != nil {
			return err
		}

		remote := headersData["remote"].(*url.URL)
		sendHeaders := headersData["sendHeaders"].(http.Header)
		passHeaders := headersData["passHeaders"].([]string)
		passStatus := headersData["passStatus"].([]int)
		forwardHeaders := headersData["forwardHeaders"].([]string)

		loadForwardedHeaders(forwardHeaders, sendHeaders, request)

		response, err := bareFetch(request, sendHeaders, remote, options)
		if err != nil {
			return err
		}

		responseHeaders := make(http.Header)

		for _, header := range passHeaders {
			if values := response.Header.Get(header); values != "" {
				responseHeaders.Set(header, values)
			}
		}

		status := http.StatusOK
		if containsInt(passStatus, response.StatusCode) {
			status = response.StatusCode
		}

		if status != cacheNotModified {
			responseHeaders.Set("x-bare-status", strconv.Itoa(response.StatusCode))
			responseHeaders.Set("x-bare-status-text", response.Status)
			m := make(map[string]interface{})

			for header, v := range response.Header {
				if strings.ToLower(header) == "set-cookie" {
					m[header] = v
				} else {
					m[header] = response.Header.Get(header)
				}
			}

			headersJSON, err := json.Marshal(m)
			if err != nil {
				fmt.Fprintf(os.Stderr, "%s\n", err)
				return &BareError{500, "UNKNOWN", "unknown", err.Error(), ""}
			}
			responseHeaders.Set("x-bare-headers", string(headersJSON))
		}

		for key, values := range splitHeaders(responseHeaders) {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}

		w.WriteHeader(status)
		io.Copy(w, response.Body)
		response.Body.Close()
		return nil
	})

	server.HandleSocket("/v3/", func(request *BareRequest, clientConn *websocket.Conn, options *Options) error {

		defer clientConn.Close()

		messageType, message, err := clientConn.ReadMessage()
		if err != nil {
			return fmt.Errorf("error reading initial message from client: %w", err)
		}

		if messageType != websocket.TextMessage {
			return errors.New("the first WebSocket message was not a text frame")
		}

		var connectPacket SocketClientToServer
		if err := json.Unmarshal(message, &connectPacket); err != nil {
			return fmt.Errorf("error unmarshalling client connection packet: %w", err)
		}

		if connectPacket.Type != "connect" {
			return errors.New("client did not send open packet")
		}

		connectHeaders := make(http.Header)
		for a, v := range connectPacket.Headers {
			connectHeaders.Set(a, v)
		}
		loadForwardedHeaders(connectPacket.ForwardHeaders, connectHeaders, request)

		parsedURL, err := url.Parse(connectPacket.Remote)
		if err != nil {
			return fmt.Errorf("error parsing remote WebSocket url: %w", err)
		}

		connectHeaders.Del("upgrade")
		connectHeaders.Del("connection")

		resp, remoteSocket, _, berr := webSocketFetch(connectHeaders, parsedURL, connectPacket.Protocols, options)
		if berr != nil {
			return fmt.Errorf("error establishing remote WebSocket connection: %w", err)
		}

		openPacket := SocketServerToClient{
			Type:       "open",
			Protocol:   remoteSocket.Subprotocol(),
			SetCookies: []string{},
		}

		openPacket.SetCookies = append(openPacket.SetCookies, resp.Header.Values("set-cookie")...)

		openPacketJSON, _ := json.Marshal(openPacket)

		if err := clientConn.WriteMessage(websocket.TextMessage, openPacketJSON); err != nil {
			return fmt.Errorf("error sending open packet to client: %w", err)
		}

		go func() {
			for {
				messageType, message, err := remoteSocket.ReadMessage()
				if err != nil {
					remoteSocket.Close()
					clientConn.Close()
					if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
						if options.LogErrors {
							fmt.Fprintf(os.Stderr, "Error reading message from remote WebSocket: %v\n", err)
						}
					}
					return
				}
				if err := clientConn.WriteMessage(messageType, message); err != nil {
					remoteSocket.Close()
					clientConn.Close()
					if options.LogErrors {
						fmt.Fprintf(os.Stderr, "Error writing message to client WebSocket: %v\n", err)
					}
					return
				}
			}
		}()

		for {
			messageType, message, err := clientConn.ReadMessage()
			if err != nil {
				remoteSocket.Close()
				clientConn.Close()
				if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
					if options.LogErrors {
						fmt.Fprintf(os.Stderr, "Error reading message from client WebSocket: %v\n", err)
					}
				}
				return nil
			}

			if err := remoteSocket.WriteMessage(messageType, message); err != nil {
				remoteSocket.Close()
				clientConn.Close()
				if options.LogErrors {
					fmt.Fprintf(os.Stderr, "Error writing message to remote WebSocket: %v\n", err)
				}
				return nil
			}
		}
	})

	server.versions = append(server.versions, "v3")
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

func contains(s []string, e string) bool {
	for _, a := range s {
		if a == e {
			return true
		}
	}
	return false
}

func containsInt(s []int, e int) bool {
	for _, a := range s {
		if a == e {
			return true
		}
	}
	return false
}

func main() {
	var directory string
	var host string
	var port int
	var errors bool
	var localAddress string
	var family int
	var maintainer string
	var maintainerFile string

	var rootCmd = &cobra.Command{
		Use:     "bare-server",
		Short:   "Bare server implementation in Go",
		Version: "0.1.0",
		Run: func(cmd *cobra.Command, args []string) {
			if maintainer != "" && maintainerFile != "" {
				fmt.Fprintln(os.Stderr, "Error: Specify either -m or -mf, not both.")
				os.Exit(1)
			}

			var maintainerData *BareMaintainer
			if maintainer != "" {
				if err := json.Unmarshal([]byte(maintainer), &maintainerData); err != nil {
					fmt.Fprintf(os.Stderr, "Error parsing maintainer data: %s\n", err)
					os.Exit(1)
				}
			} else if maintainerFile != "" {
				data, err := os.ReadFile(maintainerFile)
				if err != nil {
					fmt.Fprintf(os.Stderr, "Error reading maintainer file: %s\n", err)
					os.Exit(1)
				}
				if err := json.Unmarshal(data, &maintainerData); err != nil {
					fmt.Fprintf(os.Stderr, "Error parsing maintainer data: %s\n", err)
					os.Exit(1)
				}
			}

			options := &Options{
				LogErrors:    errors,
				LocalAddress: localAddress,
				Family:       family,
				Maintainer:   maintainerData,
			}

			bareServer := NewBareServer(directory, options)

			registerV3(bareServer)

			fmt.Printf("Error Logging: %t\n", errors)
			fmt.Printf("URL:           http://%s:%d%s\n", host, port, directory)
			if maintainerData != nil {
				fmt.Printf("Maintainer:    %s\n", maintainerData)
			}

			if err := bareServer.Start(fmt.Sprintf("%s:%d", host, port)); err != nil {
				fmt.Fprintf(os.Stderr, "Error starting server: %s\n", err)
				os.Exit(1)
			}
		},
	}

	rootCmd.Flags().StringVarP(&directory, "directory", "d", "/", "Bare directory")
	rootCmd.Flags().StringVarP(&host, "host", "o", "0.0.0.0", "Listening host")
	rootCmd.Flags().IntVarP(&port, "port", "p", 80, "Listening port")
	rootCmd.Flags().BoolVarP(&errors, "errors", "e", false, "Error logging")
	rootCmd.Flags().StringVarP(&localAddress, "local-address", "a", "", "Address/network interface")
	rootCmd.Flags().IntVarP(&family, "family", "f", 0, "IP address family used when looking up host/hostnames. Default is 0 (both IPv4 and IPv6)")
	rootCmd.Flags().StringVarP(&maintainer, "maintainer", "m", "", "Inline maintainer data (JSON)")
	rootCmd.Flags().StringVarP(&maintainerFile, "maintainer-file", "j", "", "Path to maintainer data (JSON)")

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
