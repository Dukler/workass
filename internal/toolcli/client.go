// Package toolcli is the direct Workass tool client. It speaks ordinary JSON
// over pinned loopback HTTPS, independent of a provider's MCP implementation.
package toolcli

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const MaxResponseBytes = 32 * 1024 * 1024

// Config stays in a private session context file. Credentials never appear in
// prompts, command arguments, tool catalogs, or error messages.
type Config struct {
	Endpoint   string `json:"endpoint"`
	CAFile     string `json:"ca_file"`
	Credential string `json:"credential"`
	ChatID     string `json:"chat_id"`
	TabID      string `json:"tab_id"`
}

type Call struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

type Image struct {
	Data     string `json:"data"`
	MIMEType string `json:"mime_type"`
}

type Response struct {
	Result any     `json:"result,omitempty"`
	Images []Image `json:"images,omitempty"`
	Error  string  `json:"error,omitempty"`
}

func ReadConfig(path string) (Config, error) {
	var config Config
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 64*1024 {
		return config, errors.New("Workass context file is unavailable or invalid")
	}
	f, err := os.Open(path)
	if err != nil {
		return config, errors.New("cannot open Workass context file")
	}
	defer f.Close()
	if err := json.NewDecoder(io.LimitReader(f, 64*1024)).Decode(&config); err != nil {
		return Config{}, errors.New("invalid Workass context file")
	}
	return config, nil
}

// Do issues exactly one request, with no retry or discovery handshake. In
// particular an uncertain mutation must be read back with its same operation id.
func Do(ctx context.Context, config Config, name string, call *Call) (Response, error) {
	u, err := url.Parse(config.Endpoint)
	if err != nil || u.Scheme != "https" || u.Hostname() != "tools.localhost" || u.Port() == "" || u.Path != "/workass/tools" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return Response{}, errors.New("invalid Workass tool endpoint")
	}
	if config.Credential == "" || strings.ContainsAny(config.Credential, "\r\n") || config.ChatID == "" || config.TabID == "" {
		return Response{}, errors.New("Workass session context is incomplete")
	}
	pem, err := os.ReadFile(config.CAFile)
	if err != nil {
		return Response{}, errors.New("cannot read Workass public certificate")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		return Response{}, errors.New("invalid Workass public certificate")
	}
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: "tools.localhost", MinVersion: tls.VersionTLS13},
		// Connect directly: neither DNS resolution nor a machine HTTP proxy may
		// route the session capability off the owning machine.
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", u.Port()))
		},
		TLSHandshakeTimeout: 5 * time.Second,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("Workass tool redirects are forbidden") }}
	method := http.MethodGet
	var body io.Reader
	if call != nil {
		method = http.MethodPost
		encoded, err := json.Marshal(call)
		if err != nil {
			return Response{}, errors.New("invalid Workass tool arguments")
		}
		body = bytes.NewReader(encoded)
	} else if name != "" {
		q := u.Query()
		q.Set("name", name)
		u.RawQuery = q.Encode()
	}
	request, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return Response{}, errors.New("cannot create Workass tool request")
	}
	request.Header.Set("Authorization", "Bearer "+config.Credential)
	request.Header.Set("X-Workass-Chat-ID", config.ChatID)
	request.Header.Set("X-Workass-Tab-ID", config.TabID)
	request.Header.Set("Content-Type", "application/json")
	reply, err := client.Do(request)
	if err != nil {
		return Response{}, errors.New("Workass tool connection failed or was cancelled")
	}
	defer reply.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(reply.Body, MaxResponseBytes+1))
	if err != nil || len(raw) > MaxResponseBytes {
		return Response{}, errors.New("Workass tool response is unreadable or too large")
	}
	var response Response
	if err := json.Unmarshal(raw, &response); err != nil {
		return Response{}, errors.New("invalid Workass tool response")
	}
	if reply.StatusCode < 200 || reply.StatusCode >= 300 {
		if response.Error == "" {
			response.Error = "Workass tool request rejected"
		}
	}
	return response, nil
}
