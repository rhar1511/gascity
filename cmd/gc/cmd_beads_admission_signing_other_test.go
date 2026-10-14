//go:build !aix && !android && !darwin && !dragonfly && !freebsd && !illumos && !ios && !linux && !netbsd && !openbsd && !solaris

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestBeadsAdmissionAttachRefusesUnsupportedOwnershipWithoutWriting(t *testing.T) {
	fixture := newBeadsAdmissionCommandFixture(t)
	var stdout, stderr bytes.Buffer
	if code := run([]string{
		"--city", fixture.cityPath, "beads", "admission", "prepare", fixture.source.ID,
		"--store-ref", "rig:pilot", "--route", "pilot/worker", "--deliverable", "reviewed patch",
		"--verification", "tests", "--acceptance-authority", "reviewer",
	}, &stdout, &stderr); code != 0 {
		t.Fatalf("prepare exit %d: %s", code, stderr.String())
	}
	draftPath := filepath.Join(fixture.cityPath, "reviewed-admission.json")
	if err := os.WriteFile(draftPath, stdout.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	code := run([]string{
		"--city", fixture.cityPath, "beads", "admission", "attach", draftPath,
		"--store-ref", "rig:pilot", "--key-file", fixture.keyPath,
	}, &stdout, &stderr)
	if code == 0 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "key directories require trusted ownership") {
		t.Fatalf("unsupported ownership: exit %d stdout %s stderr %s", code, stdout.String(), stderr.String())
	}
	after, err := fixture.store.Get(fixture.source.ID)
	if err != nil || !reflect.DeepEqual(fixture.source, after) || len(fixture.store.q43Receipts) != 0 || len(fixture.store.patchReceipts) != 0 || fixture.store.sourceGenericWrites != 0 {
		t.Fatalf("refused attachment wrote source or transition evidence: %+v, %v", after, err)
	}
}
