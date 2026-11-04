package main

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"github.com/charmbracelet/x/ansi"
)

func TestRegionFilterStartsAtFirstMatchAndPreservesNavigation(t *testing.T) {
	regions := []string{
		"australiacentral", "australiacentral2", "australiaeast", "austriaeast",
		"belgiumcentral", "brazilsouth", "brazilsoutheast", "brazilus",
		"canadacentral", "canadaeast", "centralindia", "centralus", "centraluseuap",
		"chilecentral", "denmarkeast", "eastasia", "eastus", "eastus2", "francecentral",
		"francesouth", "indiasouthcentral", "koreasouth", "southafricanorth",
		"southafricawest", "southcentralus", "southeastasia", "southindia", "uksouth",
	}
	value := "eastus2"
	form := huh.NewForm(huh.NewGroup(newRegionSelector(&value, regions))).WithWidth(80)
	form.Init()
	send := func(msg tea.Msg) { form.Update(msg) }
	typeText := func(text string) {
		for _, r := range text {
			send(tea.KeyPressMsg{Code: r, Text: string(r)})
		}
	}
	typeText("/south")
	if !strings.Contains(form.View(), "brazilsouth") || !strings.Contains(form.View(), "southcentralus") {
		t.Fatalf("filter hid earlier matches below the old scroll offset:\n%s", form.View())
	}
	send(tea.KeyPressMsg{Code: tea.KeyDown})
	send(tea.KeyPressMsg{Code: tea.KeyEnter})
	if value != "brazilsoutheast" {
		t.Fatalf("query did not start at the first match, or arrow navigation was reset: %q", value)
	}
}

func TestRegionFilterEditingReturnsToFirstMatch(t *testing.T) {
	for _, tc := range []struct {
		name, query, suffix string
		keys                []tea.KeyPressMsg
	}{
		{"backspace after navigating", "/south", "", []tea.KeyPressMsg{{Code: tea.KeyDown}, {Code: tea.KeyBackspace}}},
		{"clear and retype", "/south", "south", []tea.KeyPressMsg{{Code: tea.KeyDown}, {Code: 'u', Mod: tea.ModCtrl}}},
		{"recover from no matches", "/southzzz", "", []tea.KeyPressMsg{
			{Code: tea.KeyDown}, {Code: tea.KeyBackspace}, {Code: tea.KeyBackspace}, {Code: tea.KeyBackspace},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value := "uksouth"
			form := huh.NewForm(huh.NewGroup(newRegionSelector(&value,
				[]string{"eastus2", "southcentralus", "southindia", "uksouth"}))).WithWidth(80)
			form.Init()
			typeText := func(text string) {
				for _, r := range text {
					form.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
				}
			}
			typeText(tc.query)
			for _, k := range tc.keys {
				form.Update(k)
			}
			typeText(tc.suffix)
			if !strings.Contains(form.View(), "southcentralus") {
				t.Fatalf("edited query hid its first match:\n%s", form.View())
			}
			form.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			if value != "southcentralus" {
				t.Fatalf("editing the query left a stale selection: %q", value)
			}
		})
	}
}

func TestRegionFilterDoesNotPadTheFormBelowMatches(t *testing.T) {
	value, confirmed := "eastus", false
	regions := []string{"eastus", "indiasouthcentral", "southcentralus", "southcentralusstg", "westus"}
	region := newRegionSelector(&value, regions)
	region.WithHeight(12)
	form := huh.NewForm(huh.NewGroup(
		region,
		huh.NewConfirm().Title("Next field").Value(&confirmed),
	)).WithWidth(60)
	form.Init()
	for _, r := range "/southcent" {
		form.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	lines := strings.Split(ansi.Strip(form.View()), "\n")
	last, next := -1, -1
	for i, line := range lines {
		if strings.Contains(line, "southcentralusstg") {
			last = i
		}
		if strings.Contains(line, "Next field") {
			next = i
		}
	}
	if last < 0 || next != last+2 {
		t.Fatalf("filtered matches end on row %d but the next field starts on row %d:\n%s", last, next, form.View())
	}
}
