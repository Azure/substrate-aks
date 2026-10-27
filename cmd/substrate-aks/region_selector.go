package main

import (
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
)

type regionSelector struct {
	*huh.Select[string]
	editing textinput.KeyMap
}

func newRegionSelector(value *string, regions []string) huh.Field {
	return &regionSelector{
		Select: huh.NewSelect[string]().
			Title("Azure region").Description("Press / to filter regions, then Enter to select.").
			Options(huh.NewOptions(regions...)...).Value(value).Height(10).
			Validate(func(s string) error {
				_, err := validateAzureRegion(s, regions)
				return err
			}),
		editing: textinput.DefaultKeyMap(),
	}
}

func (s *regionSelector) Update(msg tea.Msg) (huh.Model, tea.Cmd) {
	filtering := s.GetFiltering()
	_, cmd := s.Select.Update(msg)
	k, ok := msg.(tea.KeyPressMsg)
	if !ok || !filtering || !s.GetFiltering() {
		return s, cmd
	}
	if k.Text == "" && !key.Matches(k,
		s.editing.DeleteWordBackward, s.editing.DeleteWordForward,
		s.editing.DeleteAfterCursor, s.editing.DeleteBeforeCursor,
		s.editing.DeleteCharacterBackward, s.editing.DeleteCharacterForward) {
		return s, cmd
	}
	// huh retains the old cursor and scroll offset when the query changes.
	// Its Home action resets both, but only outside filter-editing mode.
	s.Filtering(false)
	_, reset := s.Select.Update(tea.KeyPressMsg{Code: tea.KeyHome})
	s.Filtering(true)
	return s, tea.Batch(cmd, reset)
}
