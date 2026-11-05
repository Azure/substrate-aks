package installer

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func imageCheckReplies(c Config, result string) []reply {
	var replies []reply
	if strings.Contains(c.Substrate.Images.Repository, ".azurecr.io/") || strings.Contains(c.Substrate.Images.Plugin, ".azurecr.io/") {
		replies = append(replies, accountReply(c), reply{contains: "group exists", output: "true"}, ownerReply(c), registryReply(c),
			acrLoginReply())
	}
	replies = append(replies, fetchReplies(c.Substrate.Source)...)
	return append(replies, reply{contains: "publish check-images", output: result})
}

func TestSavedImagesInRecreatedRegistry(t *testing.T) {
	for _, tc := range []struct {
		name, status, message string
		missing               bool
	}{
		{"valid reuse", "available", "", false},
		{"empty recreated registry", "missing", "--reset-config", true},
		{"expired credentials", "authentication", "refresh az login", false},
		{"missing pull grant", "authorization", "registry pull permissions", false},
		{"network failure", "network", "DNS, TLS", false},
		{"registry unavailable", "registry-error", "service availability", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testConfig(t)
			c.Substrate.Images.Repository = "substratetest123.azurecr.io/substrate"
			c.Substrate.Images.Plugin = "substratetest123.azurecr.io/plugin@sha256:" + strings.Repeat("a", 64)
			before := c
			image := c.Substrate.Images.Repository + "/podcertcontroller:" + c.Substrate.Images.Tag
			checked := 12
			if tc.status == "available" {
				image, checked = "", 18
			}
			report, err := json.Marshal(map[string]any{"status": tc.status, "image": image, "checked": checked, "expected": 18})
			if err != nil {
				t.Fatal(err)
			}
			r := script(t, imageCheckReplies(c, string(report))...)
			type stepResult struct {
				name string
				done bool
				err  error
			}
			var events []stepResult
			err = (App{Config: c, Runner: imageBuildRunner{r, t.TempDir()},
				ImageCheckProgress: func(name string, done bool, err error) {
					events = append(events, stepResult{name, done, err})
				},
			}).CheckImages(t.Context())
			if tc.status == "available" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.message) || errors.Is(err, errImagesMissing) != tc.missing {
				t.Fatalf("image failure was misclassified: %v", err)
			}
			steps := ImageCheckSteps()
			if len(events) != 2*len(steps) {
				t.Fatalf("image verification did not report all operation boundaries: %+v", events)
			}
			for i, name := range steps {
				start, end := events[2*i], events[2*i+1]
				if start.name != name || start.done || start.err != nil || end.name != name || !end.done {
					t.Fatalf("image step %q lost its explicit start/result: %+v", name, events)
				}
				want := error(nil)
				if i == len(steps)-1 {
					want = err
				}
				if !errors.Is(end.err, want) {
					t.Fatalf("image step %q reported %v instead of %v", name, end.err, want)
				}
			}
			if !reflect.DeepEqual(c, before) {
				t.Fatal("image verification changed configuration")
			}
			call := r.calls[len(r.calls)-1]
			args := strings.Join(call.Args, " ")
			if !strings.Contains(args, c.Substrate.Images.Plugin+" substratetest123.azurecr.io "+filepath.Join(c.workspace(), "docker")) ||
				strings.Contains(strings.Join(call.Env, " "), "DOCKER_CONFIG=") {
				t.Fatal("image verification lost owned ACR credentials or replaced external registry credentials")
			}
			for _, call := range r.calls {
				if call.ShowOutput || (strings.Contains(strings.Join(call.Args, " "), "--expose-token") && !call.SensitiveOutput) {
					t.Fatal("image verification exposes login output or credentials")
				}
			}
			temporary, err := filepath.Glob(filepath.Join(c.workspace(), "image-check-*"))
			if err != nil || len(temporary) != 0 {
				t.Fatalf("temporary checker checkout retained: %v, %v", temporary, err)
			}
		})
	}
}

func TestImageCheckRejectsIncompleteResults(t *testing.T) {
	for _, report := range []string{
		`not JSON`, `{}`, `{"status":"available","checked":1,"expected":18}`,
		`{"status":"missing","checked":1,"expected":18}`, `{"status":"available","checked":18,"expected":18} {}`,
		`{"status":"available","checked":18,"expected":18,"unexpected":true}`,
	} {
		if err := imageCheckResult([]byte(report)); err == nil {
			t.Fatalf("incomplete image check became success: %s", report)
		}
	}
}

func TestDestroyRecreateAndRecoverSavedImages(t *testing.T) {
	c := testConfig(t)
	c.Substrate.Images.Repository = "substratetest123.azurecr.io/substrate"
	c.Substrate.Images.Plugin = "substratetest123.azurecr.io/plugin@sha256:" + strings.Repeat("a", 64)
	path := filepath.Join(c.Installation.Dir, "installation.json")
	before, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, before, 0600); err != nil {
		t.Fatal(err)
	}
	destroy := func() {
		t.Helper()
		r := script(t, accountReply(c), targetReply(), reply{contains: "group exists", output: "true"}, ownerReply(c),
			reply{contains: "group delete"})
		if err := (App{Config: c, Runner: r}).Destroy(t.Context(), c.Azure.ResourceGroup); err != nil {
			t.Fatal(err)
		}
	}
	destroy()
	r := script(t, accountReply(c), targetReply(), reply{contains: "group exists", output: "false"},
		reply{contains: "deployment sub create"})
	if err := (App{Config: c, Runner: r}).ProvisionRegistry(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(saved, before) {
		t.Fatal("teardown or registry recreation silently rewrote saved images")
	}
	r = script(t, imageCheckReplies(c, `{"status":"missing","image":"substratetest123.azurecr.io/substrate/ate-idp-server:experimental","checked":1,"expected":18}`)...)
	if err := (App{Config: c, Runner: imageBuildRunner{r, t.TempDir()}}).Install(t.Context(), "ate-system"); !errors.Is(err, errImagesMissing) {
		t.Fatalf("recreated empty registry reached installation: %v", err)
	}
	r = script(t, accountReply(c), targetReply(), reply{contains: "group exists", output: "true"}, ownerReply(c))
	if err := (App{Config: c, Runner: r}).ResetImageConfig(t.Context(), path); err == nil {
		t.Fatal("recovery bypassed an existing target's install/checkpoint guards")
	}
	destroy()
	r = script(t, accountReply(c), targetReply(), reply{contains: "group exists", output: "false"})
	if err := (App{Config: c, Runner: r}).ResetImageConfig(t.Context(), path); err != nil {
		t.Fatal(err)
	}
	recovered, err := LoadConfig(path)
	if err != nil || recovered.Substrate.Images != (ImagesConfig{}) || recovered.Installation.ID != c.Installation.ID {
		t.Fatalf("complete teardown did not permit explicit image recovery: %+v, %v", recovered.Substrate.Images, err)
	}
}

func TestInstallChecksImagesBeforePreparationAndCheckpoints(t *testing.T) {
	for _, component := range []string{"ate-system", "atelet", "apiserver"} {
		t.Run(component, func(t *testing.T) {
			c := testConfig(t)
			r := script(t, imageCheckReplies(c, `{"status":"missing","image":"example.invalid/substrate/podcertcontroller:experimental","checked":12,"expected":18}`)...)
			err := (App{Config: c, Runner: imageBuildRunner{r, t.TempDir()}}).Install(t.Context(), component)
			if !errors.Is(err, errImagesMissing) {
				t.Fatalf("install did not reject stale images: %v", err)
			}
			for _, call := range r.calls {
				if len(call.Input) != 0 || call.Args[0] == "kubectl" ||
					strings.Contains(strings.Join(call.Args, " "), "deployment sub show") {
					t.Fatalf("stale images reached preparation, cluster probes, uploads or install checkpoints: %v", call.Args)
				}
			}
		})
	}
	t.Run("valid images do not bypass infrastructure guards", func(t *testing.T) {
		c := testConfig(t)
		want := errors.New("ownership lookup denied")
		replies := append(imageCheckReplies(c, `{"status":"available","image":"","checked":18,"expected":18}`),
			reply{contains: "group exists", err: want})
		r := script(t, replies...)
		err := (App{Config: c, Runner: imageBuildRunner{r, t.TempDir()}}).Install(t.Context(), "ate-system")
		if !errors.Is(err, want) {
			t.Fatalf("image validation bypassed the installation's ownership guard: %v", err)
		}
	})
}

func TestImageCheckLoginFailureIsNotMissing(t *testing.T) {
	c := testConfig(t)
	c.Substrate.Images.Repository = "substratetest123.azurecr.io/substrate"
	want := errors.New("login expired")
	r := script(t, accountReply(c), reply{contains: "group exists", output: "true"}, ownerReply(c), registryReply(c),
		reply{contains: "acr login", err: want})
	var completed []string
	err := (App{Config: c, Runner: r, ImageCheckProgress: func(name string, done bool, err error) {
		if done {
			completed = append(completed, name)
			if !errors.Is(err, want) {
				t.Errorf("registry access was presented as successful: %v", err)
			}
		}
	}}).CheckImages(t.Context())
	if !errors.Is(err, want) || errors.Is(err, errImagesMissing) {
		t.Fatalf("login failure was treated as missing images: %v", err)
	}
	if !reflect.DeepEqual(completed, ImageCheckSteps()[:1]) {
		t.Fatalf("login failure started later verification operations: %v", completed)
	}
}

func TestImageCheckStopsStepsAfterSourceFailure(t *testing.T) {
	c := testConfig(t)
	want := errors.New("source fetch failed")
	r := script(t, reply{contains: "git init"}, reply{contains: "git fetch", err: want})
	var completed []string
	err := (App{Config: c, Runner: r, ImageCheckProgress: func(name string, done bool, err error) {
		if done {
			completed = append(completed, name)
			if name == imageCheckFetch && !errors.Is(err, want) {
				t.Errorf("failed source fetch was presented as successful: %v", err)
			}
		}
	}}).CheckImages(t.Context())
	if !errors.Is(err, want) || !reflect.DeepEqual(completed, ImageCheckSteps()[:2]) {
		t.Fatalf("source failure did not stop the verification checklist: %v, %v", completed, err)
	}
	temporary, err := filepath.Glob(filepath.Join(c.workspace(), "image-check-*"))
	if err != nil || len(temporary) != 0 {
		t.Fatalf("failed source checkout was retained: %v, %v", temporary, err)
	}
}

func TestResetImagesRequiresCompleteTeardown(t *testing.T) {
	for _, state := range []string{"absent", "owned registry or partial install", "AKS with managed node group", "orphaned managed node group", "foreign", "access failure", "unknown existence", "concurrent edit", "retargeted", "target discovery failure"} {
		t.Run(state, func(t *testing.T) {
			c := testConfig(t)
			c.Substrate.Runtime.AssetsDir = "relative-assets"
			path := filepath.Join(c.Installation.Dir, "installation.json")
			before, err := json.Marshal(c)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, before, 0600); err != nil {
				t.Fatal(err)
			}
			expected, err := LoadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			replies := []reply{accountReply(c)}
			switch state {
			case "AKS with managed node group":
				replies = append(replies, reply{contains: "group list --tag",
					output: "[" + resourceGroupJSON(c, c.Azure.ResourceGroup, "") + "," + managedNodeGroupJSON(c) + "]"})
			case "orphaned managed node group":
				replies = append(replies, reply{contains: "group list --tag", output: "[" + managedNodeGroupJSON(c) + "]"})
			case "retargeted":
				replies = append(replies, reply{contains: "group list --tag",
					output: "[" + resourceGroupJSON(c, "original-group", "") + "]"})
			case "target discovery failure":
				replies = append(replies, reply{contains: "group list --tag", err: errors.New("list access denied")})
			default:
				replies = append(replies, targetReply())
			}
			switch state {
			case "absent", "concurrent edit":
				replies = append(replies, reply{contains: "group exists", output: "false"})
			case "owned registry or partial install", "AKS with managed node group":
				replies = append(replies, reply{contains: "group exists", output: "true"}, ownerReply(c))
			case "foreign":
				replies = append(replies, reply{contains: "group exists", output: "true"},
					reply{contains: "group show", output: `{"location":"eastus2","tags":{}}`})
			case "access failure":
				replies = append(replies, reply{contains: "group exists", err: errors.New("access denied")})
			case "unknown existence":
				replies = append(replies, reply{contains: "group exists", output: "null"})
			}
			if state == "concurrent edit" {
				before = bytes.ReplaceAll(before, []byte("experimental"), []byte("edited-tag"))
				if err := os.WriteFile(path, before, 0600); err != nil {
					t.Fatal(err)
				}
			}
			r := script(t, replies...)
			err = (App{Config: expected, Runner: r}).ResetImageConfig(t.Context(), path)
			if state == "AKS with managed node group" && (err == nil || !strings.Contains(err.Error(), "fully destroy it first")) {
				t.Fatalf("managed node group prevented the teardown-only reset check: %v", err)
			}
			if state != "absent" {
				after, readErr := os.ReadFile(path)
				if err == nil || readErr != nil || !bytes.Equal(after, before) {
					t.Fatalf("unsafe reset changed saved configuration: %v, %v", err, readErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			after, err := readConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			c.Substrate.Images = ImagesConfig{}
			c.Installation.Dir = ""
			if !reflect.DeepEqual(after, c) {
				t.Fatalf("reset changed identity, source, paths or non-image configuration: %+v", after)
			}
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatal("image reset relaxed config file permissions")
			}
		})
	}
}
