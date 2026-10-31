package ctl

import (
	"context"
	"encoding/json"
	"io"
	"net/http"

	"github.com/aisidev/tt/api"
	"github.com/aisidev/tt/internal/command"
	"github.com/aisidev/tt/model"
)

func (s *Server) guestConfigCapabilitiesHTTP(w http.ResponseWriter, r *http.Request) {
	api.WriteJSON(w, 200, api.Success(api.GuestConfigCapabilities{ProtocolVersion: 1, StartWithConfig: true}))
}
func (s *Server) startWithConfigHTTP(w http.ResponseWriter, r *http.Request) {
	request, ok := api.ReadJSON[api.GuestConfigStart](w, r)
	if !ok {
		return
	}
	if request.Version != 1 || len(request.Files) == 0 {
		reply(w, model.Unit{}, internal(&api.BoundaryError{Class: "terminal", Code: "invalid_request", Message: "invalid guest configuration start"}), 0)
		return
	}
	if _, ok := command.FromContext(r.Context()); !ok {
		reply(w, model.Unit{}, internal(&api.BoundaryError{Class: "denied", Code: "missing_identity", Message: "guest configuration requires a durable command"}), 0)
		return
	}
	ctx := command.CopyContext(s.workCtx, r.Context())
	reply(w, model.Unit{}, s.changeEnvironmentConfigured(ctx, r.PathValue("id"), "start", request.Files), 200)
}
func (c *agentClient) checkGuestConfig(ctx context.Context, host model.Host) error {
	response, err := c.request(ctx, "GET", host.Addr, "/api/guest-config-capabilities", nil, nil)
	if err != nil {
		return err
	}
	body, err := guestConfigResponse[api.GuestConfigCapabilities](response)
	if err != nil || response.StatusCode != 200 || !body.OK || body.Data == nil || body.Data.ProtocolVersion != 1 || !body.Data.StartWithConfig {
		return &api.BoundaryError{Class: "terminal", Code: "incompatible_protocol", Message: "host does not negotiate guest configuration start"}
	}
	return nil
}
func (c *agentClient) startWithConfig(ctx context.Context, host model.Host, id string, files model.GuestConfig) error {
	response, err := c.request(ctx, "POST", host.Addr, "/api/vms/"+id+"/start-with-config", api.GuestConfigStart{Version: 1, Files: files}, nil)
	if err != nil {
		return err
	}
	body, err := guestConfigResponse[model.Unit](response)
	if err != nil {
		return err
	}
	if response.StatusCode >= 200 && response.StatusCode < 300 && body.OK {
		return nil
	}
	if body.Failure != nil {
		return body.Failure
	}
	return &api.BoundaryError{Class: "unknown", Code: "unavailable", Message: "guest configuration start result unavailable"}
}

func guestConfigResponse[T any](response *http.Response) (api.Resp[T], error) {
	defer response.Body.Close()
	var result api.Resp[T]
	data, err := io.ReadAll(io.LimitReader(response.Body, 4097))
	if err != nil || len(data) > 4096 || json.Unmarshal(data, &result) != nil {
		return result, &api.BoundaryError{Class: "unknown", Code: "invalid_message", Message: "guest configuration response unavailable"}
	}
	return result, nil
}
