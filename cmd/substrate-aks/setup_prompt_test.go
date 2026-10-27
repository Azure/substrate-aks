package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestSetupPromptsKeepAnswersAndDefaults(t *testing.T) {
	var out bytes.Buffer
	p := newSetupPrompt(strings.NewReader("\nsouthcen\nsouthcentralus\nno\nyes\n"), newSetupUI(&out), true)
	name, err := p.text(t.Context(), "Resource group", "substrate-dev")
	if err != nil || name != "substrate-dev" {
		t.Fatalf("resource name default lost: %q, %v", name, err)
	}
	region, err := p.region(t.Context(), "eastus2", []string{"eastus2", "southcentralus"})
	if err != nil || region != "southcentralus" {
		t.Fatalf("region selection lost: %q, %v", region, err)
	}
	if !strings.Contains(out.String(), "possible matches: southcentralus") {
		t.Fatalf("invalid region did not display inline validation: %s", &out)
	}
	for _, want := range []bool{false, true} {
		value, err := p.confirm(t.Context(), "Proceed?", !want)
		if err != nil || value != want {
			t.Fatalf("confirmation answer lost: %v, %v", value, err)
		}
	}
}

func TestSetupPromptsDoNotTreatEOFAsConsent(t *testing.T) {
	for _, answer := range []string{"", "yes"} {
		var out bytes.Buffer
		p := newSetupPrompt(strings.NewReader(answer), newSetupUI(&out), true)
		if _, err := p.confirm(t.Context(), "Proceed?", true); !errors.Is(err, io.EOF) {
			t.Fatalf("missing or incomplete answer accepted: %q, %v", answer, err)
		}
	}
}

func TestAccessibleRegionRejectsEOFAndInvalidInput(t *testing.T) {
	for _, answer := range []string{"", "southcen\n"} {
		var out bytes.Buffer
		p := newSetupPrompt(strings.NewReader(answer), newSetupUI(&out), true)
		if _, err := p.region(t.Context(), "eastus2", []string{"eastus2", "southcentralus"}); !errors.Is(err, io.EOF) {
			t.Fatalf("missing region selection accepted: %q, %v", answer, err)
		}
	}
}

func TestAccessibleSetupPromptCancellation(t *testing.T) {
	in, writer := io.Pipe()
	defer in.Close()
	defer writer.Close()
	var out bytes.Buffer
	ctx, cancel := context.WithCancel(t.Context())
	p := newSetupPrompt(in, newSetupUI(&out), true)
	done := make(chan error, 1)
	go func() {
		_, err := p.confirm(ctx, "Proceed?", false)
		done <- err
	}()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation was hidden: %v", err)
	}
}

func TestAccessibleEnginePickerValidatesAndPreservesNextAnswer(t *testing.T) {
	for _, selection := range []string{"", "docker", "podman"} {
		t.Run("selection="+selection, func(t *testing.T) {
			var out bytes.Buffer
			p := newSetupPrompt(strings.NewReader("invalid\n"+selection+"\nno\n"), newSetupUI(&out), true)
			engine, err := p.containerEngine(t.Context())
			want := selection
			if want == "" {
				want = "docker"
			}
			if err != nil || engine != want || !strings.Contains(out.String(), "choose docker or podman") {
				t.Fatalf("engine selection/default/validation lost: %q, %v\n%s", engine, err, &out)
			}
			proceed, err := p.confirm(t.Context(), "Proceed?", true)
			if err != nil || proceed {
				t.Fatalf("engine picker consumed the next confirmation answer: %v, %v", proceed, err)
			}
		})
	}
}

func TestAccessibleEnginePickerRejectsEOFAndCancellation(t *testing.T) {
	for _, answer := range []string{"", "podman", "invalid\n"} {
		var out bytes.Buffer
		p := newSetupPrompt(strings.NewReader(answer), newSetupUI(&out), true)
		if _, err := p.containerEngine(t.Context()); !errors.Is(err, io.EOF) {
			t.Fatalf("incomplete engine selection accepted: %q, %v", answer, err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var out bytes.Buffer
	p := newSetupPrompt(strings.NewReader("podman\n"), newSetupUI(&out), true)
	if _, err := p.containerEngine(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("engine picker lost cancellation: %v", err)
	}
}
