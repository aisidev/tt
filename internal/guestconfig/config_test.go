package guestconfig

import (
	"github.com/aisidev/tt/model"
	"strings"
	"testing"
)

func TestRejectsPathsAndUnsupportedEnginesWithoutEchoingSecrets(t *testing.T) {
	for _, name := range []string{"../escape", "/absolute", "a/b", ".hidden", "x\nsecret"} {
		err := Validate(model.Firecracker, model.GuestConfig{name: "private-content"}, false)
		if err == nil || strings.Contains(err.Error(), "private-content") {
			t.Fatal(name, err)
		}
	}
	files := model.GuestConfig{"app.json": "{}"}
	if err := Validate(model.Firecracker, files, true); err != nil {
		t.Fatal(err)
	}
	if Validate(model.Qemu, files, false) == nil || Validate(model.Docker, nil, true) == nil || Validate(model.Firecracker, model.GuestConfig{"a": strings.Repeat("x", MaxConfigBytes)}, false) == nil {
		t.Fatal("accepted unsupported or oversized input")
	}
}
func TestDigestDistinguishesContentsAndFileBoundaries(t *testing.T) {
	if Digest(nil) != nil {
		t.Fatal("empty digest")
	}
	if *Digest(model.GuestConfig{"a": "bc"}) == *Digest(model.GuestConfig{"ab": "c"}) {
		t.Fatal("boundary collision")
	}
}
func TestConfigDiskAccountingIncludesSSHOnly(t *testing.T) {
	if DiskMiB(model.Firecracker, false, true) != 4 || DiskMiB(model.Qemu, true, true) != 0 || DiskMiB(model.Firecracker, false, false) != 0 {
		t.Fatal("config disk reservation")
	}
}
