package ctl

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/aisidev/tt/internal/command"
	"github.com/aisidev/tt/model"
)

// Enrollment token issuance is local operator work, never a tenant HTTP action.
// Tokens are published only in private files; stdout contains public paths.
func enrollmentTokenCLI(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "init" && args[0] != "issue" {
		fmt.Fprintln(stderr, "Usage: tt-ctl enrollment-token init --parent DIR | issue --directory DIR --authority-file FILE --request-file FILE --key UUID")
		return 2
	}
	fs := flag.NewFlagSet("enrollment-token", flag.ContinueOnError)
	fs.SetOutput(stderr)
	parent := fs.String("parent", "", "Existing private parent for a new generated grant directory")
	directory := fs.String("directory", "", "Original generated grant directory")
	authorityPath := fs.String("authority-file", "", "Selected receiver authority file")
	requestPath := fs.String("request-file", "", "Private reviewed enrollment request")
	key := fs.String("key", "", "Original issuance UUID")
	if fs.Parse(args[1:]) != nil || fs.NArg() != 0 {
		return 2
	}
	if args[0] == "init" {
		store, e := command.CreateEnrollmentStore(*parent, *key)
		if e != nil {
			fmt.Fprintln(stderr, "enrollment store creation failed")
			return 1
		}
		_ = json.NewEncoder(stdout).Encode(map[string]any{"version": 1, "directory": store.Directory})
		return 0
	}
	authority, e := command.OpenAuthority(*authorityPath)
	if e != nil {
		fmt.Fprintln(stderr, "receiver authority unavailable")
		return 1
	}
	state, e := authority.State()
	if e != nil {
		return 1
	}
	file, e := os.OpenFile(*requestPath, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if e != nil {
		return 1
	}
	defer file.Close()
	info, e := file.Stat()
	if e != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 4096 {
		return 1
	}
	var request model.EnrollmentRequest
	decoder := json.NewDecoder(io.LimitReader(file, 4097))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&request) != nil || decoder.Decode(new(any)) != io.EOF {
		return 2
	}
	store := command.EnrollmentStore{Directory: *directory}
	if _, e = store.Issue(state.ID, state.Epoch, *key, request); e != nil {
		fmt.Fprintln(stderr, "enrollment token issuance failed")
		return 1
	}
	_ = json.NewEncoder(stdout).Encode(map[string]any{"version": 1, "directory": *directory, "key": *key, "token_file": filepath.Join(*directory, *key+".token")})
	return 0
}
