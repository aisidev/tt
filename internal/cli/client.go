package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/aisidev/tt/api"
	"github.com/aisidev/tt/internal/auth"
	"github.com/aisidev/tt/model"
)

type Client struct {
	base                         string
	http                         *http.Client
	key                          *string
	readTimeout, mutationTimeout time.Duration
}

func newClient(address string, key *string) (*Client, error) {
	if key != nil {
		if _, err := auth.ParseAPIKey(*key); err != nil {
			return nil, err
		}
	}
	base := address
	if !strings.HasPrefix(address, "http") {
		base = "http://" + address
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DisableKeepAlives = true
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	transport.Protocols = protocols
	return &Client{base: base, http: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, key: key, readTimeout: 60 * time.Second, mutationTimeout: 600 * time.Second}, nil
}

const mutationFailure = "request failed; a submitted operation may still be running — inspect the environment before retrying"

func (c *Client) request(ctx context.Context, method, path string, body any, headers http.Header, timeout time.Duration) (*http.Response, context.CancelFunc, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	var reader io.Reader
	if body != nil {
		data, err := model.SerdeJSON(body)
		if err != nil {
			cancel()
			return nil, nil, err
		}
		reader = bytes.NewReader(data)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	if c.key != nil {
		request.Header.Set("Authorization", "Bearer "+*c.key)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	for name, values := range headers {
		request.Header[name] = append([]string{}, values...)
	}
	response, err := c.http.Do(request)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	return response, cancel, nil
}
func decode[T any](response *http.Response) (api.Resp[T], error) {
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		return api.Resp[T]{}, err
	}
	var value api.Resp[T]
	if err := json.Unmarshal(data, &value); err != nil {
		return value, err
	}
	return value, nil
}
func httpStatus(code int) string { return fmt.Sprintf("%d %s", code, http.StatusText(code)) }
func success(code int) bool      { return code >= 200 && code < 300 }
func fetch[T any](ctx context.Context, c *Client, method, path string, body any) (T, error) {
	var zero T
	timeout, label := c.readTimeout, "read request failed"
	if method != "GET" {
		timeout, label = c.mutationTimeout, mutationFailure
	}
	response, cancel, err := c.request(ctx, method, path, body, nil, timeout)
	if err != nil {
		return zero, fmt.Errorf("%s: %w", label, err)
	}
	defer cancel()
	status := response.StatusCode
	result, err := decode[T](response)
	if err != nil {
		return zero, fmt.Errorf("invalid response (HTTP %s): %w", httpStatus(status), err)
	}
	if success(status) && result.OK {
		if result.Data == nil {
			return zero, fmt.Errorf("empty response")
		}
		return *result.Data, nil
	}
	if result.Error != nil {
		return zero, fmt.Errorf("%s", *result.Error)
	}
	return zero, fmt.Errorf("HTTP %s", httpStatus(status))
}
func (c *Client) action(ctx context.Context, method, path string) error {
	response, cancel, err := c.request(ctx, method, path, nil, nil, c.mutationTimeout)
	if err != nil {
		return fmt.Errorf("%s: %w", mutationFailure, err)
	}
	defer cancel()
	status := response.StatusCode
	result, err := decode[model.Unit](response)
	if err != nil {
		return fmt.Errorf("invalid response (HTTP %s): %w", httpStatus(status), err)
	}
	if success(status) && result.OK {
		return nil
	}
	if result.Error != nil {
		return fmt.Errorf("%s", *result.Error)
	}
	return fmt.Errorf("HTTP %s", httpStatus(status))
}
func (c *Client) backup(ctx context.Context, id string, request model.BackupRequest) (api.BackupView, error) {
	method, suffix := "PUT", ""
	if request.Action == model.BackupRestore {
		method = "POST"
		suffix = "/restore"
	} else if request.Action == model.BackupDelete {
		method = "DELETE"
	}
	headers := http.Header{}
	headers.Set("Idempotency-Key", request.OperationID)
	headers.Set("If-Match", "\""+request.ExpectedRevision+"\"")
	var body any
	if request.Generation != nil {
		body = api.RestoreBody{Generation: *request.Generation}
	}
	response, cancel, err := c.request(ctx, method, "/api/vms/"+id+"/backup"+suffix, body, headers, c.mutationTimeout)
	if err != nil {
		return api.BackupView{}, fmt.Errorf("backup outcome unknown; inspect and use the printed exact retry: %w", err)
	}
	defer cancel()
	status := response.StatusCode
	result, err := decode[api.BackupView](response)
	if err != nil {
		return api.BackupView{}, fmt.Errorf("backup outcome unknown: invalid response: %w", err)
	}
	if success(status) && result.OK {
		if result.Data == nil {
			return api.BackupView{}, fmt.Errorf("backup response missing status")
		}
		return *result.Data, nil
	}
	if result.Error != nil {
		return api.BackupView{}, fmt.Errorf("%s", *result.Error)
	}
	return api.BackupView{}, fmt.Errorf("backup request failed: %s", httpStatus(status))
}
