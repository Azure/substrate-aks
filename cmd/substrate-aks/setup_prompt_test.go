package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"github.com/charmbracelet/x/ansi"
)

func TestSetupClusterNameInputValidationAndPlaceholder(t *testing.T) {
	for _, tc := range []struct {
		name string
		ok   bool
	}{
		{"", false}, {"ab", false}, {"abc", true}, {"substrate-dev", true},
		{"Uppercase", false}, {"under_score", false}, {"9cluster", false}, {"cluster-", false},
		{strings.Repeat("a", 40), true}, {strings.Repeat("a", 41), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value := tc.name
			field := newSetupClusterNameInput(&value, nil)
			form := huh.NewForm(huh.NewGroup(field))
			form.WithWidth(80)
			view := ansi.Strip(field.View())
			if tc.name == "" {
				if !strings.Contains(view, "substrate-dev") {
					t.Fatalf("empty name lost its example placeholder:\n%s", view)
				}
			} else if strings.Contains(view, "Valid cluster name.") != tc.ok {
				t.Fatalf("editing feedback did not reflect the name:\n%s", view)
			}
			field.Focus()
			_, cmd := field.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			if (field.Error() == nil) != tc.ok || (!tc.ok && cmd != nil) {
				t.Fatalf("invalid name advanced or valid name rejected: %q, %v", value, field.Error())
			}
			if value != tc.name {
				t.Fatalf("name was silently changed or truncated: %q", value)
			}
		})
	}
}

func TestSetupClusterNameInputFeedbackFollowsEdits(t *testing.T) {
	value := ""
	field := newSetupClusterNameInput(&value, nil)
	field.Focus()
	field.Update(tea.PasteMsg{Content: "bad_name"})
	if view := field.View(); !strings.Contains(view, "Invalid name:") {
		t.Fatalf("invalid paste did not update feedback before submission:\n%s", view)
	}
	field.Update(tea.KeyPressMsg{Code: 'u', Mod: tea.ModCtrl})
	field.Update(tea.PasteMsg{Content: "valid-name"})
	if view := field.View(); value != "valid-name" || !strings.Contains(view, "Valid cluster name.") || strings.Contains(view, "Invalid name:") {
		t.Fatalf("correction left stale validation feedback: %q\n%s", value, view)
	}
}

func TestSetupPromptsKeepAnswersAndDefaults(t *testing.T) {
	var out bytes.Buffer
	p := newSetupPrompt(strings.NewReader("Bad_Name\n\nsouthcen\nsouthcentralus\nno\nyes\n"), newSetupUI(&out), true)
	name, region, err := p.installation(t.Context(), "substrate-dev", "eastus2", []string{"eastus2", "southcentralus"}, nil)
	if err != nil || name != "substrate-dev" {
		t.Fatalf("resource name default lost: %q, %v", name, err)
	}
	if err != nil || region != "southcentralus" {
		t.Fatalf("region selection lost: %q, %v", region, err)
	}
	if !strings.Contains(out.String(), "possible matches: southcentralus") {
		t.Fatalf("invalid region did not display inline validation: %s", &out)
	}
	if !strings.Contains(out.String(), "must be 3-40") {
		t.Fatalf("invalid cluster name did not show accessible validation: %s", &out)
	}
	for _, want := range []bool{false, true} {
		value, err := p.confirm(t.Context(), "Proceed?", !want)
		if err != nil || value != want {
			t.Fatalf("confirmation answer lost: %v, %v", value, err)
		}
	}
}

func TestAccessibleInstallationFormRejectsIncompleteAnswers(t *testing.T) {
	for _, answer := range []string{"", "\n", "\nsouthcen\n", "\nsouthcentralus"} {
		var out bytes.Buffer
		p := newSetupPrompt(strings.NewReader(answer), newSetupUI(&out), true)
		if _, _, err := p.installation(t.Context(), "substrate-dev", "eastus2", []string{"eastus2", "southcentralus"}, nil); !errors.Is(err, io.EOF) {
			t.Fatalf("incomplete installation form accepted: %q, %v", answer, err)
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
