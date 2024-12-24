package bare

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/gorilla/websocket"
)

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
