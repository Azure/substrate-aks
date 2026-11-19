package installer

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestRestoreCollisionDoesNotAdoptOrDelete(t *testing.T) {
	c := testConfig(t)
	namespace, actor, token, err := restoreNames()
	if err != nil {
		t.Fatal(err)
	}

	collision := errors.New("already exists")
	r := &restoreRunner{config: c, namespace: namespace, actor: actor, token: token,
		failures: map[string]error{" create -f - --output=json": collision}}
	state := &restoreState{namespace: namespace, token: token, client: "/client"}
	pool := []byte(fmt.Sprintf("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: %s\n---\napiVersion: ate.dev/v1\nkind: WorkerPool\nmetadata:\n  name: counter-microvm\n  namespace: %s\n", namespace, namespace))
	err = (App{Config: c, Runner: r}).restoreRun(t.Context(), t.Context(), state, actor, pool, []byte("template"))
	if !errors.Is(err, collision) || !strings.Contains(err.Error(), "uncertain") {
		t.Fatalf("collision was not retained: %v", err)
	}
	if strings.Contains(strings.Join(r.calls, "\n"), " delete ") {
		t.Fatal("collision was deleted")
	}
}

func TestRestoreAtespaceCollisionDoesNotAdoptOrDelete(t *testing.T) {
	c := testConfig(t)
	namespace, actor, token, err := restoreNames()
	if err != nil {
		t.Fatal(err)
	}
	collision := errors.New("already exists")
	r := &restoreRunner{config: c, namespace: namespace, actor: actor, token: token,
		failures: map[string]error{" create atespace ": collision}}
	state := &restoreState{namespace: namespace, token: token, client: "/client"}
	pool := []byte(fmt.Sprintf("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: %s\n---\napiVersion: ate.dev/v1\nkind: WorkerPool\nmetadata:\n  name: counter-microvm\n  namespace: %s\n", namespace, namespace))
	err = (App{Config: c, Runner: r}).restoreRun(t.Context(), t.Context(), state, actor, pool, []byte("template"))
	if !errors.Is(err, collision) || !strings.Contains(err.Error(), "uncertain") {
		t.Fatalf("collision was not retained: %v", err)
	}
	if strings.Contains(strings.Join(r.calls, "\n"), " delete ") {
		t.Fatal("colliding atespace was deleted")
	}
}
