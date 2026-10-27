package ctl

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
	"github.com/aisidev/tt/internal/command"
	"github.com/aisidev/tt/model"
)

type agentClient struct {
	http *http.Client
	key  *string
}

func newAgentClient(key *string, timeout time.Duration) (*agentClient, error) {
	if key != nil {
		if _, err := auth.ParseAPIKey(*key); err != nil {
			return nil, err
		}
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DisableKeepAlives = true
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	transport.Protocols = protocols
	return &agentClient{http: &http.Client{Transport: transport, Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, key: key}, nil
}
func (c *agentClient) request(ctx context.Context, method, address, path string, body any, headers http.Header) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		data, err := model.SerdeJSON(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(data)
	}
	request, err := http.NewRequestWithContext(ctx, method, "http://"+address+path, reader)
	if err != nil {
		return nil, err
	}
	if c.key != nil {
		request.Header.Set("Authorization", "Bearer "+*c.key)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if envelope, ok := command.FromContext(ctx); ok && method != "GET" {
		parts := strings.Split(strings.Trim(path, "/"), "/")
		identity := ""
		if len(parts) >= 3 {
			identity = parts[2]
		}
		if create, ok := body.(api.CreateVmReq); ok {
			identity = create.VmID
		}
		if disk, ok := body.(model.VolumeSpec); ok {
			identity = disk.ID
		}
		envelope.Key += "/" + identity
		data, _ := json.Marshal(envelope)
		request.Header.Set("X-Compute-Command", string(data))
	}

	for key, values := range headers {
		request.Header[key] = append([]string{}, values...)
	}
	return c.http.Do(request)
}
func decodeResponse[T any](response *http.Response) (api.Resp[T], error) {
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		return api.Resp[T]{}, err
	}
	var result api.Resp[T]
	if err := json.Unmarshal(data, &result); err != nil {
		return result, err
	}
	return result, nil
}
func statusText(code int) string { return fmt.Sprintf("%d %s", code, http.StatusText(code)) }
func successful(code int) bool   { return code >= 200 && code < 300 }
func (c *agentClient) create(ctx context.Context, address string, request api.CreateVmReq) (*model.Vm, error) {
	response, err := c.request(ctx, "POST", address, "/api/vms", request, nil)
	if err != nil {
		return nil, err
	}
	status := response.StatusCode
	result, err := decodeResponse[api.CreateVmResp](response)
	if err != nil {
		return nil, fmt.Errorf("agent HTTP %s: invalid response: %w", statusText(status), err)
	}
	if result.Failure != nil {
		return nil, result.Failure
	}
	if !successful(status) || !result.OK {
		if result.Error != nil {
			return nil, fmt.Errorf("%s", *result.Error)
		}
		return nil, fmt.Errorf("agent HTTP %s", statusText(status))
	}
	if result.Data == nil {
		return nil, fmt.Errorf("agent returned no VM")
	}
	vm := result.Data.Vm
	if vm.ID != request.VmID || vm.EnvID != request.EnvID {
		return nil, fmt.Errorf("agent returned an unexpected VM identity")
	}
	return &vm, nil
}
func (c *agentClient) action(ctx context.Context, host model.Host, id string, action *string, options ...model.StopOptions) error {
	method, path := "DELETE", "/api/vms/"+id
	if action != nil {
		method = "POST"
		path += "/" + *action
	}
	var body any
	if action != nil && *action == "stop" && len(options) > 0 {
		body = options[0]
	}
	response, err := c.request(ctx, method, host.Addr, path, body, nil)
	if err != nil {
		return err
	}
	status := response.StatusCode
	result, err := decodeResponse[model.Unit](response)
	if err != nil {
		return fmt.Errorf("agent HTTP %s: invalid response: %w", statusText(status), err)
	}
	if successful(status) && result.OK {
		return nil
	}
	if result.Failure != nil {
		return result.Failure
	}
	if result.Error != nil {
		return fmt.Errorf("%s", *result.Error)
	}
	return fmt.Errorf("agent HTTP %s", statusText(status))
}

type hostSnapshot struct {
	info api.AgentInfo
	vms  []model.Vm
	err  error
}

func (c *agentClient) snapshot(ctx context.Context, host model.Host) hostSnapshot {
	response, err := c.request(ctx, "GET", host.Addr, "/api/info", nil, nil)
	if err != nil {
		return hostSnapshot{err: err}
	}
	if response.StatusCode >= 400 {
		response.Body.Close()
		return hostSnapshot{err: fmt.Errorf("agent HTTP %s", statusText(response.StatusCode))}
	}
	result, err := decodeResponse[api.AgentInfo](response)
	if err != nil {
		return hostSnapshot{err: err}
	}
	if !result.OK || result.Data == nil {
		return hostSnapshot{err: fmt.Errorf("invalid agent info")}
	}
	info := *result.Data
	if info.HostID != host.ID {
		return hostSnapshot{err: fmt.Errorf("agent identity changed; re-register host")}
	}
	if info.VMs != nil {
		vms := []model.Vm(*info.VMs)
		info.VMs = nil
		return hostSnapshot{info: info, vms: vms}
	}
	response, err = c.request(ctx, "GET", host.Addr, "/api/vms", nil, nil)
	if err != nil {
		return hostSnapshot{err: err}
	}
	if response.StatusCode >= 400 {
		response.Body.Close()
		return hostSnapshot{err: fmt.Errorf("agent HTTP %s", statusText(response.StatusCode))}
	}
	inventory, err := decodeResponse[model.List[model.Vm]](response)
	if err != nil {
		return hostSnapshot{err: err}
	}
	if !inventory.OK || inventory.Data == nil {
		return hostSnapshot{err: fmt.Errorf("invalid VM snapshot")}
	}
	return hostSnapshot{info: info, vms: []model.Vm(*inventory.Data)}
}
