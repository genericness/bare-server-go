// SPDX-License-Identifier: MPL-2.0
// tomphttp/bare-server-go
package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
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
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/spf13/cobra"
)

const (
	maxHeaderValue = 3072
)

type BareError struct {
	Status int    `json:"status"`
	Code   string `json:"code"`
	ID     string `json:"id"`
	Message string `json:"message,omitempty"`
	Stack   string `json:"stack,omitempty"`
}

func (e *BareError) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

type BareServer struct {
	directory string
	routes    map[string]RouteCallback
	socketRoutes map[string]SocketRouteCallback
	versions  []string
	closed    bool
	options   *Options
	wss       *websocket.Upgrader
	db        *JSONDatabaseAdapter
}

type Options struct {
	LogErrors    bool
	FilterRemote func(*url.URL) error
	Lookup       func(hostname string, service string, hints ...net.IPAddr) (addrs []net.IPAddr, err error)
	LocalAddress string
	Family       int
	Maintainer   *BareMaintainer
	httpAgent    *http.Transport
	httpsAgent   *http.Transport
}

type RouteCallback func(request *BareRequest, response http.ResponseWriter, options *Options) (*Response, error)

type SocketRouteCallback func(request *BareRequest, conn *websocket.Conn, options *Options) error

type BareRequest struct {
	*http.Request
	Native *http.Request
}

type Response struct {
	StatusCode int
	Status     string
	Headers    http.Header
	Body       io.Reader
}

func (r *Response) Write(w http.ResponseWriter) error {
	for key, values := range r.Headers {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(r.StatusCode)
	if r.Body != nil {
		_, err := io.Copy(w, r.Body)
		return err
	}
	return nil
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
	LanguageDeno         BareLanguage = "Deno"
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
	Maintainer  *BareMaintainer  `json:"maintainer,omitempty"`
	Project     *BareProject     `json:"project,omitempty"`
	Versions    []string        `json:"versions"`
	Language    BareLanguage    `json:"language"`
	MemoryUsage float64         `json:"memoryUsage,omitempty"`
}

func NewBareServer(directory string, options *Options) *BareServer {
	if options.LogErrors == false {
		options.LogErrors = false
	}

	if options.FilterRemote == nil {
		options.FilterRemote = func(remote *url.URL) error {
			if isValidIP(remote.Hostname()) && parseIP(remote.Hostname()).IsGlobalUnicast() == false {
				return errors.New("forbidden IP")
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

	server := &BareServer{
		directory: directory,
		routes:    make(map[string]RouteCallback),
		socketRoutes: make(map[string]SocketRouteCallback),
		versions:  make([]string, 0),
		options:   options,
		wss: &websocket.Upgrader{
			ReadBufferSize:  1024,
			WriteBufferSize: 1024,
			CheckOrigin: func(r *http.Request) bool {
				return true
			},
		},
		db: NewJSONDatabaseAdapter(NewMemoryDatabase()),
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
	var response *Response
	var err error

	defer func() {
		if err != nil {
			if s.options.LogErrors {
				fmt.Fprintf(os.Stderr, "Error handling request: %s\n", err)
			}

			if httpErr, ok := err.(error); ok {
				if strings.HasPrefix(httpErr.Error(), "404") {
					http.Error(w, httpErr.Error(), http.StatusNotFound)
				} else {
					http.Error(w, httpErr.Error(), http.StatusInternalServerError)
				}
			} else {
				http.Error(w, err.Error(), http.StatusInternalServerError)
			}

			return
		}
		if response != nil {
			if err := response.Write(w); err != nil {
				if s.options.LogErrors {
					fmt.Fprintf(os.Stderr, "Error writing response: %s\n", err)
				}
			}
		}
	}()

	if r.Method == http.MethodOptions {
		response = &Response{
			StatusCode: http.StatusOK,
			Headers:    make(http.Header),
		}
	} else if service == "/" {
		response = &Response{
			StatusCode: http.StatusOK,
			Headers:    make(http.Header),
			Body:       s.getInstanceInfo(),
		}
	} else if handler, ok := s.routes[service]; ok {
		response, err = handler(request, w, s.options)
	} else {
		err = createHttpError(http.StatusNotFound, "Not Found")
	}
}

func (s *BareServer) getInstanceInfo() io.Reader {
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

	jsonData, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		panic(err)
	}

	return bytes.NewReader(jsonData)
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

func randomHex(byteLength int) string {
	bytes := make([]byte, byteLength)
	if _, err := rand.Read(bytes); err != nil {
		panic(err)
	}
	return hex.EncodeToString(bytes)
}

func outgoingError(err error) error {
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
	return err
}

func bareFetch(request *BareRequest, requestHeaders http.Header, remote *url.URL, options *Options) (*http.Response, error) {
	if options.FilterRemote != nil {
		if err := options.FilterRemote(remote); err != nil {
			return nil, err
		}
	}

	req, err := http.NewRequest(request.Method, remote.String(), request.Body)
	if err != nil {
		return nil, err
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

func bareUpgradeFetch(request *BareRequest, requestHeaders http.Header, remote *url.URL, options *Options) (*http.Response, *tls.Conn, error) {
	if options.FilterRemote != nil {
		if err := options.FilterRemote(remote); err != nil {
			return nil, nil, err
		}
	}

	dialer := &net.Dialer{
		LocalAddr: getLocalAddr(options.LocalAddress, options.Family),
		Timeout:   12 * time.Second,
	}

	var conn net.Conn
	var err error

	if remote.Scheme == "wss" {
		conn, err = tls.DialWithDialer(dialer, "tcp", remote.Host, &tls.Config{
			InsecureSkipVerify: true,
		})
	} else {
		conn, err = dialer.Dial("tcp", remote.Host)
	}

	if err != nil {
		return nil, nil, outgoingError(err)
	}

	req := &http.Request{
		Method:     request.Method,
		URL:        remote,
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     requestHeaders,
		Body:       request.Body,
		Host:       remote.Host,
	}

	if err := req.Write(conn); err != nil {
		return nil, nil, err
	}

	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, req)
	if err != nil {
		return nil, nil, err
	}

	return resp, conn.(*tls.Conn), nil
}

func webSocketFetch(request *BareRequest, requestHeaders http.Header, remote *url.URL, protocols []string, options *Options) (*http.Response, *websocket.Conn, *http.Request,  error) {
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

func remoteToURL(remote map[string]string) (*url.URL, error) {
	port := remote["port"]
	if port == "" {
		if remote["protocol"] == "http:" {
			port = "80"
		} else if remote["protocol"] == "https:" {
			port = "443"
		}
	}

	urlStr := fmt.Sprintf("%s//%s:%s%s", remote["protocol"], remote["host"], port, remote["path"])
	return url.Parse(urlStr)
}

func objectFromRawHeaders(raw []string) http.Header {
	headers := make(http.Header)
	for i := 0; i < len(raw); i += 2 {
		key := raw[i]
		value := raw[i+1]
		headers.Add(key, value)
	}
	return headers
}

func rawHeaderNames(raw http.Header) []string {
	var names []string
	for name := range raw {
		names = append(names, name)
	}
	return names
}

func mapHeadersFromArray(from []string, to http.Header) http.Header {
	for _, header := range from {
		if values, ok := to[strings.ToLower(header)]; ok {
			to[header] = values
			delete(to, strings.ToLower(header))
		}
	}
	return to
}

func flattenHeader(values []string) string {
	return strings.Join(values, ", ")
}

func JSON(w http.ResponseWriter, statusCode int, data interface{}) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	return json.NewEncoder(w).Encode(data)
}

type SocketClientToServer struct {
	Type          string      `json:"type"`
	Remote        string      `json:"remote"`
	Protocols    []string    `json:"protocols"`
	Headers      http.Header `json:"headers"`
	ForwardHeaders []string    `json:"forwardHeaders"`
}

type SocketServerToClient struct {
	Type       string   `json:"type"`
	Protocol   string   `json:"protocol"`
	SetCookies []string `json:"setCookies"`
}

func registerV3(server *BareServer) {
	nullBodyStatus := []int{101, 204, 205, 304}
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

	readHeaders := func(request *BareRequest) (map[string]interface{}, error) {
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
			"passHeaders":   passHeaders,
			"passStatus":     passStatus,
			"forwardHeaders": forwardHeaders,
		}

		return result, nil
	}

	server.Handle("/v3/", func(request *BareRequest, w http.ResponseWriter, options *Options) (*Response, error) {
		headersData, err := readHeaders(request)
		if err != nil {
			return nil, err
		}

		remote := headersData["remote"].(*url.URL)
		sendHeaders := headersData["sendHeaders"].(http.Header)
		passHeaders := headersData["passHeaders"].([]string)
		passStatus := headersData["passStatus"].([]int)
		forwardHeaders := headersData["forwardHeaders"].([]string)

		loadForwardedHeaders(forwardHeaders, sendHeaders, request)

		response, err := bareFetch(request, sendHeaders, remote, options)
		if err != nil {
			return nil, err
		}
		defer response.Body.Close()

		responseHeaders := make(http.Header)

		for _, header := range passHeaders {
			if values := response.Header[header]; len(values) > 0 {
				responseHeaders.Set(header, flattenHeader(values))
			}
		}

		status := http.StatusOK
		if containsInt(passStatus, response.StatusCode) {
			status = response.StatusCode
		}

		if status != cacheNotModified {
			responseHeaders.Set("x-bare-status", strconv.Itoa(response.StatusCode))
			responseHeaders.Set("x-bare-status-text", response.Status)
			headersToPass := mapHeadersFromArray(rawHeaderNames(response.Header), response.Header)
			headersJSON, err := json.Marshal(headersToPass)
			if err != nil {
				return nil, err
			}
			responseHeaders.Set("x-bare-headers", string(headersJSON))
		}

		responseBody := io.Reader(response.Body)
		if !containsInt(nullBodyStatus, status) {
			responseBody = response.Body
		}

		return &Response{
			StatusCode: status,
			Headers:    splitHeaders(responseHeaders),
			Body:       responseBody,
		}, nil
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
	
		loadForwardedHeaders(connectPacket.ForwardHeaders, connectPacket.Headers, request)
	
		_, remoteSocket, httpReq, err := webSocketFetch(request, connectPacket.Headers, &url.URL{Scheme: "wss", Host: connectPacket.Remote}, connectPacket.Protocols, options)
		if err != nil {
			return fmt.Errorf("error establishing remote WebSocket connection: %w", err)
		}
		defer remoteSocket.Close()
	
		openPacket := SocketServerToClient{
			Type:       "open",
			Protocol:   remoteSocket.Subprotocol(),
			SetCookies: httpReq.Header["Set-Cookie"], 
		}
		openPacketJSON, _ := json.Marshal(openPacket)
	
		if err := clientConn.WriteMessage(websocket.TextMessage, openPacketJSON); err != nil {
			return fmt.Errorf("error sending open packet to client: %w", err)
		}
	
		go func() {
			defer func() {
				clientConn.Close()
				remoteSocket.Close()
			}()
	
			for {
				messageType, message, err := remoteSocket.ReadMessage()
				if err != nil {
					if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
						if options.LogErrors {
							fmt.Fprintf(os.Stderr, "Error reading message from remote WebSocket: %v\n", err)
						}
					}
					return 
				}
				if err := clientConn.WriteMessage(messageType, message); err != nil {
					if options.LogErrors {
						fmt.Fprintf(os.Stderr, "Error writing message to client WebSocket: %v\n", err)
					}
					return 
				}
			}
		}()
	
		defer func() {
			clientConn.Close()
			remoteSocket.Close()
		}() 
	
		for {
			messageType, message, err := clientConn.ReadMessage()
			if err != nil {
				if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
					if options.LogErrors {
						fmt.Fprintf(os.Stderr, "Error reading message from client WebSocket: %v\n", err)
					}
				}
				return nil 
			}
	
			if err := remoteSocket.WriteMessage(messageType, message); err != nil {
				if options.LogErrors {
					fmt.Fprintf(os.Stderr, "Error writing message to remote WebSocket: %v\n", err)
				}
				return nil 
			}
		}
	})
	server.versions = append(server.versions, "v3")
}


func registerV2(server *BareServer) {
	nullBodyStatus := []int{101, 204, 205, 304}
	forbiddenSendHeaders := []string{"connection", "content-length", "transfer-encoding"}
	forbiddenForwardHeaders := []string{"connection", "transfer-encoding", "host", "origin", "referer"}
	forbiddenPassHeaders := []string{"vary", "connection", "transfer-encoding", "access-control-allow-headers", "access-control-allow-methods", "access-control-expose-headers", "access-control-max-age", "access-control-request-headers", "access-control-request-method"}
	defaultForwardHeaders := []string{"accept-encoding", "accept-language", "sec-websocket-extensions", "sec-websocket-key", "sec-websocket-version"}
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

	readHeaders := func(request *BareRequest) (map[string]interface{}, error) {
		remote := make(map[string]string)
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

		for _, remoteProp := range []string{"host", "port", "protocol", "path"} {
			header := fmt.Sprintf("x-bare-%s", remoteProp)
			value := headers.Get(header)
			if value == "" {
				return nil, &BareError{400, "MISSING_BARE_HEADER", fmt.Sprintf("request.headers.%s", header), "Header was not specified.", ""}
			}

			switch remoteProp {
			case "port":
				if _, err := strconv.Atoi(value); err != nil {
					return nil, &BareError{400, "INVALID_BARE_HEADER", fmt.Sprintf("request.headers.%s", header), "Header was not a valid integer.", ""}
				}
			case "protocol":
				if !contains([]string{"http:", "https:", "ws:", "wss:"}, value) {
					return nil, &BareError{400, "INVALID_BARE_HEADER", fmt.Sprintf("request.headers.%s", header), "Header was invalid.", ""}
				}
			}
			remote[remoteProp] = value
		}

		xBareHeaders := headers.Get("x-bare-headers")
		if xBareHeaders == "" {
			return nil, &BareError{400, "MISSING_BARE_HEADER", "request.headers.x-bare-headers", "Header was not specified.", ""}
		}

		var jsonHeaders map[string]interface{}
		if err := json.Unmarshal([]byte(xBareHeaders), &jsonHeaders); err != nil {
			return nil, &BareError{400, "INVALID_BARE_HEADER", "request.headers.x-bare-headers", fmt.Sprintf("Header contained invalid JSON. (%s)", err.Error()), ""}
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

		remoteURL, err := remoteToURL(remote)
		if err != nil {
			return nil, &BareError{400, "INVALID_BARE_HEADER", "request.headers.x-bare-(host|port|protocol|path)", "Invalid remote.", ""}
		}
		result := map[string]interface{}{
			"remote":         remoteURL,
			"sendHeaders":    sendHeaders,
			"passHeaders":   passHeaders,
			"passStatus":     passStatus,
			"forwardHeaders": forwardHeaders,
		}

		return result, nil
	}

	server.Handle("/v2/", func(request *BareRequest, w http.ResponseWriter, options *Options) (*Response, error) {
		headersData, err := readHeaders(request)
		if err != nil {
			return nil, err
		}

		remote := headersData["remote"].(*url.URL)
		sendHeaders := headersData["sendHeaders"].(http.Header)
		passHeaders := headersData["passHeaders"].([]string)
		passStatus := headersData["passStatus"].([]int)
		forwardHeaders := headersData["forwardHeaders"].([]string)

		loadForwardedHeaders(forwardHeaders, sendHeaders, request)

		response, err := bareFetch(request, sendHeaders, remote, options)
		if err != nil {
			return nil, err
		}
		defer response.Body.Close()

		responseHeaders := make(http.Header)
		for _, header := range passHeaders {
			if values := response.Header[header]; len(values) > 0 {
				responseHeaders.Set(header, flattenHeader(values))
			}
		}

		status := http.StatusOK
		if containsInt(passStatus, response.StatusCode) {
			status = response.StatusCode
		}

		if status != cacheNotModified {
			responseHeaders.Set("x-bare-status", strconv.Itoa(response.StatusCode))
			responseHeaders.Set("x-bare-status-text", response.Status)
			headersToPass := mapHeadersFromArray(rawHeaderNames(response.Header), response.Header)
			headersJSON, err := json.Marshal(headersToPass)
			if err != nil {
				return nil, err
			}
			responseHeaders.Set("x-bare-headers", string(headersJSON))
		}

		responseBody := io.Reader(nil)
		if !containsInt(nullBodyStatus, status) {
			responseBody = response.Body
		}

		return &Response{
			StatusCode: status,
			Headers:    splitHeaders(responseHeaders),
			Body:       responseBody,
		}, nil
	})

	metaExpiration := 30 * time.Second

	server.Handle("/v2/ws-meta", func(request *BareRequest, w http.ResponseWriter, options *Options) (*Response, error) {
		if request.Method == http.MethodOptions {
			return &Response{
				StatusCode: http.StatusOK,
				Headers:    make(http.Header),
			}, nil
		}

		id := request.Header.Get("x-bare-id")
		if id == "" {
			return nil, &BareError{400, "MISSING_BARE_HEADER", "request.headers.x-bare-id", "Header was not specified.", ""}
		}

		meta, err := server.db.Get(id)
		if err != nil {
			return nil, &BareError{500, "DATABASE_ERROR", "database", "Failed to retrieve metadata.", ""}
		}

		if meta == "" {
			return nil, &BareError{400, "INVALID_BARE_HEADER", "request.headers.x-bare-id", "Unregistered ID.", ""}
		}

		var metaV2 *MetaV2
		if err := json.Unmarshal([]byte(meta), &metaV2); err != nil {
			return nil, &BareError{500, "DATABASE_ERROR", "database", "Failed to parse metadata.", ""}
		}

		if metaV2.V != 2 {
			return nil, &BareError{400, "INVALID_BARE_HEADER", "request.headers.x-bare-id", "Unregistered ID.", ""}
		}

		if metaV2.Response == nil {
			return nil, &BareError{400, "INVALID_BARE_HEADER", "request.headers.x-bare-id", "Metadata not ready.", ""}
		}

		if err := server.db.Delete(id); err != nil {
			return nil, &BareError{500, "DATABASE_ERROR", "database", "Failed to delete metadata.", ""}
		}

		responseHeaders := make(http.Header)
		responseHeaders.Set("x-bare-status", strconv.Itoa(metaV2.Response.Status))
		responseHeaders.Set("x-bare-status-text", metaV2.Response.StatusText)
		headersJSON, err := json.Marshal(metaV2.Response.Headers)
		if err != nil {
			return nil, err
		}
		responseHeaders.Set("x-bare-headers", string(headersJSON))

		return &Response{
			StatusCode: http.StatusOK,
			Headers:    splitHeaders(responseHeaders),
		}, nil
	})

	server.Handle("/v2/ws-new-meta", func(request *BareRequest, w http.ResponseWriter, options *Options) (*Response, error) {
		headersData, err := readHeaders(request)
		if err != nil {
			return nil, err
		}

		remote := headersData["remote"].(*url.URL)
		sendHeaders := headersData["sendHeaders"].(http.Header)
		forwardHeaders := headersData["forwardHeaders"].([]string)

		id := randomHex(16)
		meta := &MetaV2{
			V:              2,
			Remote:         remote.String(),
			SendHeaders:    sendHeaders,
			ForwardHeaders: forwardHeaders,
		}

		metaJSON, err := json.Marshal(meta)
		if err != nil {
			return nil, err
		}

		if err := server.db.Set(id, string(metaJSON), metaExpiration); err != nil {
			return nil, &BareError{500, "DATABASE_ERROR", "database", "Failed to store metadata.", ""}
		}

		return &Response{
			StatusCode: http.StatusOK,
			Body:       bytes.NewReader([]byte(id)),
		}, nil
	})

	server.HandleSocket("/v2/", func(request *BareRequest, socket *websocket.Conn, options *Options) error {
		defer socket.Close()
	
		if request.Header.Get("Sec-Websocket-Protocol") == "" {
			return nil 
		}
	
		id := request.Header.Get("Sec-Websocket-Protocol")
		meta, err := server.db.Get(id)
		if err != nil {
			return &BareError{500, "DATABASE_ERROR", "database", "Failed to retrieve metadata.", ""}
		}
	
		if meta == "" {
			return nil 
		}
	
		var metaV2 *MetaV2
		if err := json.Unmarshal([]byte(meta), &metaV2); err != nil {
			return &BareError{500, "DATABASE_ERROR", "database", "Failed to parse metadata.", ""}
		}
	
		if metaV2.V != 2 {
			return nil 
		}
	
		loadForwardedHeaders(metaV2.ForwardHeaders, metaV2.SendHeaders, request)
	
		remoteURL, err := url.Parse(metaV2.Remote)
		if err != nil {
			return &BareError{400, "INVALID_BARE_HEADER", "request.headers.x-bare-(host|port|protocol|path)", "Invalid remote.", ""}
		}
	
		remoteResponse, remoteConn, err := bareUpgradeFetch(request, metaV2.SendHeaders, remoteURL, options)
		if err != nil {
			return err
		}
		defer remoteConn.Close()
	
		wsConn, _, err := websocket.NewClient(remoteConn, remoteURL, request.Header, 1024, 1024)
		if err != nil {
			return fmt.Errorf("error upgrading to WebSocket: %w", err)
		}
		defer wsConn.Close()
	
		metaV2.Response = &MetaV2Response{
			Status:     remoteResponse.StatusCode,
			StatusText: remoteResponse.Status,
			Headers:    remoteResponse.Header,
		}
		updatedMetaJSON, err := json.Marshal(metaV2)
		if err != nil {
			return err 
		}
		if err := server.db.Set(id, string(updatedMetaJSON), metaExpiration); err != nil {
			return &BareError{500, "DATABASE_ERROR", "database", "Failed to store metadata.", ""}
		}
	
		responseHeaders := fmt.Sprintf(
			"HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Protocol: %s\r\n",
			id,
		)
		if extensions := remoteResponse.Header.Get("Sec-Websocket-Extensions"); extensions != "" {
			responseHeaders += fmt.Sprintf("Sec-WebSocket-Extensions: %s\r\n", extensions)
		}
		if accept := remoteResponse.Header.Get("Sec-WebSocket-Accept"); accept != "" {
			responseHeaders += fmt.Sprintf("Sec-WebSocket-Accept: %s\r\n", accept)
		}
		responseHeaders += "\r\n"
	
		if err := socket.WriteMessage(websocket.TextMessage, []byte(responseHeaders)); err != nil {
			return fmt.Errorf("error writing response headers: %w", err)
		}

		go func() {
			defer func() {
				socket.Close()
				remoteConn.Close()
				wsConn.Close() 
			}()
	
			for {
				messageType, message, err := wsConn.ReadMessage()
				if err != nil {
					if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
						if options.LogErrors {
							fmt.Fprintf(os.Stderr, "Error reading message from remote WebSocket: %v\n", err)
						}
					}
					return 
				}
	
				if err := socket.WriteMessage(messageType, message); err != nil {
					if options.LogErrors {
						fmt.Fprintf(os.Stderr, "Error writing message to client WebSocket: %v\n", err)
					}
					return 
				}
			}
		}()
	
		defer func() {
			socket.Close()
			remoteConn.Close()
			wsConn.Close()
		}()
	
		for {
			messageType, message, err := socket.ReadMessage()
			if err != nil {
				if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
					if options.LogErrors {
						fmt.Fprintf(os.Stderr, "Error reading message from client WebSocket: %v\n", err)
					}
				}
				return nil
			}
	
			if err := wsConn.WriteMessage(messageType, message); err != nil {
				if options.LogErrors {
					fmt.Fprintf(os.Stderr, "Error writing message to remote WebSocket: %v\n", err)
				}
				return nil 
			}
		}
	})

	server.versions = append(server.versions, "v2")
}


type MetaV1 struct {
	V        int                   `json:"v"`
	Response *MetaV1Response `json:"response,omitempty"`
}

type MetaV1Response struct {
	Headers http.Header `json:"headers"`
}

type MetaV2 struct {
	V            int                   `json:"v"`
	Response     *MetaV2Response `json:"response,omitempty"`
	SendHeaders  http.Header         `json:"sendHeaders"`
	Remote       string              `json:"remote"`
	ForwardHeaders []string            `json:"forwardHeaders"`
}

type MetaV2Response struct {
	Status     int         `json:"status"`
	StatusText string      `json:"statusText"`
	Headers    http.Header `json:"headers"`
}

func registerV1(server *BareServer) {
	forbiddenSendHeaders := []string{"connection", "content-length", "transfer-encoding"}
	forbiddenForwardHeaders := []string{"connection", "transfer-encoding", "origin", "referer"}

	loadForwardedHeaders := func(forward []string, target http.Header, request *BareRequest) {
		for _, header := range forward {
			if value := request.Header.Get(header); value != "" {
				target.Set(header, value)
			}
		}
	}

	readHeaders := func(request *BareRequest) (map[string]interface{}, error) {
		remote := make(map[string]string)
		headers := make(http.Header)

		for _, remoteProp := range []string{"host", "port", "protocol", "path"} {
			header := fmt.Sprintf("x-bare-%s", remoteProp)
			value := request.Header.Get(header)
			if value == "" {
				return nil, &BareError{400, "MISSING_BARE_HEADER", fmt.Sprintf("request.headers.%s", header), "Header was not specified.", ""}
			}

			switch remoteProp {
			case "port":
				if _, err := strconv.Atoi(value); err != nil {
					return nil, &BareError{400, "INVALID_BARE_HEADER", fmt.Sprintf("request.headers.%s", header), "Header was not a valid integer.", ""}
				}
			case "protocol":
				if !contains([]string{"http:", "https:", "ws:", "wss:"}, value) {
					return nil, &BareError{400, "INVALID_BARE_HEADER", fmt.Sprintf("request.headers.%s", header), "Header was invalid.", ""}
				}
			}

			remote[remoteProp] = value
		}

		xBareHeaders := request.Header.Get("x-bare-headers")
		if xBareHeaders == "" {
			return nil, &BareError{400, "MISSING_BARE_HEADER", "request.headers.x-bare-headers", "Header was not specified.", ""}
		}

		var jsonHeaders map[string]interface{}
		if err := json.Unmarshal([]byte(xBareHeaders), &jsonHeaders); err != nil {
			return nil, &BareError{400, "INVALID_BARE_HEADER", "request.headers.x-bare-headers", fmt.Sprintf("Header contained invalid JSON. (%s)", err.Error()), ""}
		}

		for header, value := range jsonHeaders {
			if contains(forbiddenSendHeaders, strings.ToLower(header)) {
				continue
			}

			switch v := value.(type) {
			case string:
				headers.Set(header, v)
			case []interface{}:
				for _, v := range v {
					if strVal, ok := v.(string); ok {
						headers.Add(header, strVal)
					} else {
						return nil, &BareError{400, "INVALID_BARE_HEADER", fmt.Sprintf("bare.headers.%s", header), "Header value must be a string or an array of strings.", ""}
					}
				}
			default:
				return nil, &BareError{400, "INVALID_BARE_HEADER", fmt.Sprintf("bare.headers.%s", header), "Header value must be a string or an array of strings.", ""}
			}
		}

		xBareForwardHeaders := request.Header.Get("x-bare-forward-headers")
		if xBareForwardHeaders == "" {
			return nil, &BareError{400, "MISSING_BARE_HEADER", "request.headers.x-bare-forward-headers", "Header was not specified.", ""}
		}

		var forwardHeaders []string
		if err := json.Unmarshal([]byte(xBareForwardHeaders), &forwardHeaders); err != nil {
			return nil, &BareError{400, "INVALID_BARE_HEADER", "request.headers.x-bare-forward-headers", fmt.Sprintf("Header contained invalid JSON. (%s)", err.Error()), ""}
		}

		for i, header := range forwardHeaders {
			forwardHeaders[i] = strings.ToLower(header)
		}

		for _, header := range forbiddenForwardHeaders {
			if contains(forwardHeaders, header) {
				return nil, &BareError{400, "FORBIDDEN_BARE_HEADER", "request.headers.x-bare-forward-headers", "A forbidden header was passed.", ""}
			}
		}

		loadForwardedHeaders(forwardHeaders, headers, request)

		remoteURL, err := remoteToURL(remote)
		if err != nil {
			return nil, &BareError{400, "INVALID_BARE_HEADER", "request.headers.x-bare-(host|port|protocol|path)", "Invalid remote.", ""}
		}

		return map[string]interface{}{
			"remote":         remoteURL,
			"headers":        headers,
			"forwardHeaders": forwardHeaders,
		}, nil
	}

	server.Handle("/v1/", func(request *BareRequest, w http.ResponseWriter, options *Options) (*Response, error) {
		headersData, err := readHeaders(request)
		if err != nil {
			return nil, err
		}

		remote := headersData["remote"].(*url.URL)
		headers := headersData["headers"].(http.Header)

		response, err := bareFetch(request, headers, remote, options)
		if err != nil {
			return nil, err
		}
		defer response.Body.Close()

		responseHeaders := make(http.Header)

		for header, values := range response.Header {
			if header == "Content-Encoding" || header == "X-Content-Encoding" {
				responseHeaders.Set("Content-Encoding", flattenHeader(values))
			} else if header == "Content-Length" {
				responseHeaders.Set("Content-Length", flattenHeader(values))
			}
		}

		responseHeaders.Set("x-bare-headers", response.Header.Get("x-bare-headers"))
		responseHeaders.Set("x-bare-status", strconv.Itoa(response.StatusCode))
		responseHeaders.Set("x-bare-status-text", response.Status)

		return &Response{
			StatusCode: http.StatusOK,
			Headers:    responseHeaders,
			Body:       response.Body,
		}, nil
	})

	metaExpiration := 30 * time.Second

	server.Handle("/v1/ws-meta", func(request *BareRequest, w http.ResponseWriter, options *Options) (*Response, error) {
		if request.Method == http.MethodOptions {
			return &Response{
				StatusCode: http.StatusOK,
				Headers:    make(http.Header),
			}, nil
		}

		id := request.Header.Get("x-bare-id")
		if id == "" {
			return nil, &BareError{400, "MISSING_BARE_HEADER", "request.headers.x-bare-id", "Header was not specified.", ""}
		}

		meta, err := server.db.Get(id)
		if err != nil {
			return nil, &BareError{500, "DATABASE_ERROR", "database", "Failed to retrieve metadata.", ""}
		}
		if meta == "" {
			return nil, &BareError{400, "INVALID_BARE_HEADER", "request.headers.x-bare-id", "Unregistered ID.", ""}
		}

		var metaV1 *MetaV1
		if err := json.Unmarshal([]byte(meta), &metaV1); err != nil {
			return nil, &BareError{500, "DATABASE_ERROR", "database", "Failed to parse metadata.", ""}
		}

		if metaV1.V != 1 {
			return nil, &BareError{400, "INVALID_BARE_HEADER", "request.headers.x-bare-id", "Unregistered ID.", ""}
		}

		if err := server.db.Delete(id); err != nil {
			return nil, &BareError{500, "DATABASE_ERROR", "database", "Failed to delete metadata.", ""}
		}

		return &Response{
			StatusCode: http.StatusOK,
			Headers:    make(http.Header),
			Body:       bytes.NewReader([]byte(`{"headers":` + meta + `}`)),
		}, nil
	})

	server.Handle("/v1/ws-new-meta", func(request *BareRequest, w http.ResponseWriter, options *Options) (*Response, error) {
		id := randomHex(16)
		meta := &MetaV1{
			V: 1,
		}
		metaJSON, _ := json.Marshal(meta)
		if err := server.db.Set(id, string(metaJSON), metaExpiration); err != nil {
			return nil, &BareError{500, "DATABASE_ERROR", "database", "Failed to store metadata.", ""}
		}

		return &Response{
			StatusCode: http.StatusOK,
			Body:       bytes.NewReader([]byte(id)),
		}, nil
	})

	server.HandleSocket("/v1/", func(request *BareRequest, socket *websocket.Conn, options *Options) error {
		defer socket.Close()
	
		if request.Header.Get("Sec-Websocket-Protocol") == "" {
			return nil 
		}
	
		parts := strings.SplitN(request.Header.Get("Sec-Websocket-Protocol"), ",", 2)
		if len(parts) != 2 || strings.TrimSpace(parts[0]) != "bare" {
			return nil 
		}
	
		var metaData struct {
			Remote         map[string]string `json:"remote"`
			Headers        http.Header       `json:"headers"`
			ForwardHeaders []string          `json:"forward_headers"`
			ID             string            `json:"id"`
		}
	
		data, err := decodeProtocol(strings.TrimSpace(parts[1]))
		if err != nil {
			return fmt.Errorf("error decoding protocol data: %w", err)
		}
	
		if err := json.Unmarshal([]byte(data), &metaData); err != nil {
			return fmt.Errorf("error unmarshalling metadata: %w", err)
		}
	
		loadForwardedHeaders(metaData.ForwardHeaders, metaData.Headers, request)
	
		remoteURL, err := remoteToURL(metaData.Remote)
		if err != nil {
			return fmt.Errorf("error parsing remote URL: %w", err)
		}
	
		remoteResponse, remoteConn, err := bareUpgradeFetch(request, metaData.Headers, remoteURL, options)
		if err != nil {
			return fmt.Errorf("error upgrading to websocket: %w", err)
		}
		defer remoteConn.Close()
	
		wsConn, _, err := websocket.NewClient(remoteConn, remoteURL, request.Header, 1024, 1024)
		if err != nil {
			return fmt.Errorf("error upgrading to WebSocket: %w", err)
		}
		defer wsConn.Close()
	
		if metaData.ID != "" {
			meta, err := server.db.Get(metaData.ID)
			if err != nil {
				return &BareError{500, "DATABASE_ERROR", "database", "Failed to retrieve metadata.", ""}
			}
			if meta != "" {
				var metaV1 *MetaV1
				if err := json.Unmarshal([]byte(meta), &metaV1); err != nil {
					return &BareError{500, "DATABASE_ERROR", "database", "Failed to parse metadata.", ""}
				}
				if metaV1.V == 1 {
					metaV1.Response = &MetaV1Response{
						Headers: remoteResponse.Header,
					}
					updatedMetaJSON, err := json.Marshal(metaV1)
					if err != nil {
						return err
					}
					if err := server.db.Set(metaData.ID, string(updatedMetaJSON), metaExpiration); err != nil {
						return &BareError{500, "DATABASE_ERROR", "database", "Failed to store metadata.", ""}
					}
				}
			}
		}
	
		responseHeaders := fmt.Sprintf(
			"HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Protocol: bare\r\nSec-WebSocket-Accept: %s\r\n",
			remoteResponse.Header.Get("Sec-WebSocket-Accept"),
		)
		if extensions := remoteResponse.Header.Get("Sec-Websocket-Extensions"); extensions != "" {
			responseHeaders += fmt.Sprintf("Sec-WebSocket-Extensions: %s\r\n", extensions)
		}
		responseHeaders += "\r\n"
	
		if err := socket.WriteMessage(websocket.TextMessage, []byte(responseHeaders)); err != nil {
			return fmt.Errorf("error writing response headers: %w", err)
		}
	
		go func() {
			defer func() {
				socket.Close()
				remoteConn.Close()
				wsConn.Close() 
			}()
	
			for {
				messageType, message, err := wsConn.ReadMessage()
				if err != nil {
					if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
						if options.LogErrors {
							fmt.Fprintf(os.Stderr, "Error reading message from remote WebSocket: %v\n", err)
						}
					}
					return
				}
	
				if err := socket.WriteMessage(messageType, message); err != nil {
					if options.LogErrors {
						fmt.Fprintf(os.Stderr, "Error writing message to client WebSocket: %v\n", err)
					}
					return 
				}
			}
		}()
	
		defer func() {
			socket.Close()
			remoteConn.Close()
			wsConn.Close()
		}() 
	
		for {
			messageType, message, err := socket.ReadMessage()
			if err != nil {
				if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
					if options.LogErrors {
						fmt.Fprintf(os.Stderr, "Error reading message from client WebSocket: %v\n", err)
					}
				}
				return nil 
			}
	
			if err := wsConn.WriteMessage(messageType, message); err != nil { 
				if options.LogErrors {
					fmt.Fprintf(os.Stderr, "Error writing message to remote WebSocket: %v\n", err)
				}
				return nil
			}
		}
	})

	server.versions = append(server.versions, "v1")
}

type Database interface {
	Get(key string) (string, error)
	Set(key string, value string, expiration time.Duration) error
	Delete(key string) error
}

type MemoryDatabase struct {
	data      map[string]string
	expirations map[string]time.Time
	mutex     sync.RWMutex
}

func NewMemoryDatabase() *MemoryDatabase {
	db := &MemoryDatabase{
		data:      make(map[string]string),
		expirations: make(map[string]time.Time),
		mutex:     sync.RWMutex{},
	}
	go db.cleanupExpiredKeys()
	return db
}

func (db *MemoryDatabase) Get(key string) (string, error) {
	db.mutex.RLock()
	defer db.mutex.RUnlock()

	if expiration, ok := db.expirations[key]; ok {
		if time.Now().After(expiration) {
			delete(db.data, key)
			delete(db.expirations, key)
			return "", nil
		}
	}

	value, ok := db.data[key]
	if !ok {
		return "", nil 
	}
	return value, nil
}

func (db *MemoryDatabase) Set(key string, value string, expiration time.Duration) error {
	db.mutex.Lock()
	defer db.mutex.Unlock()

	db.data[key] = value
	db.expirations[key] = time.Now().Add(expiration)
	return nil
}

func (db *MemoryDatabase) Delete(key string) error {
	db.mutex.Lock()
	defer db.mutex.Unlock()

	delete(db.data, key)
	delete(db.expirations, key)
	return nil
}

func (db *MemoryDatabase) cleanupExpiredKeys() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for {
		<-ticker.C

		db.mutex.Lock()
		for key, expiration := range db.expirations {
			if time.Now().After(expiration) {
				delete(db.data, key)
				delete(db.expirations, key)
			}
		}
		db.mutex.Unlock()
	}
}

type JSONDatabaseAdapter struct {
	db Database
}

func NewJSONDatabaseAdapter(db Database) *JSONDatabaseAdapter {
	return &JSONDatabaseAdapter{db: db}
}

func (jda *JSONDatabaseAdapter) Get(key string) (string, error) {
	return jda.db.Get(key)
}

func (jda *JSONDatabaseAdapter) Set(key string, value string, expiration time.Duration) error {
	return jda.db.Set(key, value, expiration)
}

func (jda *JSONDatabaseAdapter) Delete(key string) error {
	return jda.db.Delete(key)
}

func decodeProtocol(protocol string) (string, error) {
	return url.PathUnescape(protocol)
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

func createHttpError(statusCode int, message string) error {
    return fmt.Errorf("%d %s", statusCode, message)
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

			registerV1(bareServer)
			registerV2(bareServer)
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
