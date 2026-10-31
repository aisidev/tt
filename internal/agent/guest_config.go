package agent

import (
	"context"
	"github.com/aisidev/tt/internal/guestconfig"
	"golang.org/x/sys/unix"
	"io"
	"net/http"
	"os"
	"runtime"

	"github.com/aisidev/tt/api"
	"github.com/aisidev/tt/internal/capability"
	"github.com/aisidev/tt/internal/command"
	"github.com/aisidev/tt/model"
)

func (s *Server) guestConfigCapabilitiesHTTP(w http.ResponseWriter, r *http.Request) {
	api.WriteJSON(w, 200, api.Success(api.GuestConfigCapabilities{ProtocolVersion: 1, StartWithConfig: runtime.GOOS == "linux" && capability.Require(s.runtime.capabilityReport(), model.Firecracker, s.runtime.storage, capability.GuestConfig) == nil}))
}
func (s *Server) startWithConfigHTTP(w http.ResponseWriter, r *http.Request) {
	request, ok := api.ReadJSON[api.GuestConfigStart](w, r)
	if !ok {
		return
	}
	if request.Version != 1 || len(request.Files) == 0 {
		failure(w, diskError("terminal", "invalid_request", "invalid guest configuration start"))
		return
	}
	if _, ok := command.FromContext(r.Context()); !ok {
		failure(w, diskError("denied", "missing_identity", "guest configuration requires a durable command"))
		return
	}
	_, err := mutate(s, func(ctx context.Context, host *Runtime) (model.Unit, error) {
		return model.Unit{}, host.startVM(ctx, r.PathValue("id"), request.Files)
	}, r.Context())
	actionReply(w, err)
}

func readOriginalSSHSeed(path string) ([]byte, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, diskError("unknown", "recovery_evidence_unavailable", "original SSH seed is unavailable")
	}
	defer file.Close()
	var stat unix.Stat_t
	if unix.Fstat(int(file.Fd()), &stat) != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&0077 != 0 || stat.Uid != uint32(os.Geteuid()) || stat.Nlink != 1 || stat.Size < 1 || stat.Size > guestconfig.MaxConfigBytes {
		return nil, diskError("unknown", "recovery_evidence_unavailable", "original SSH seed is invalid")
	}
	data, err := io.ReadAll(io.LimitReader(file, guestconfig.MaxConfigBytes+1))
	if err != nil || len(data) > guestconfig.MaxConfigBytes {
		return nil, diskError("unknown", "recovery_evidence_unavailable", "original SSH seed is unavailable")
	}
	return data, nil
}
