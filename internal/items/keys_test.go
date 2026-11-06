package items_test

import (
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/jamesainslie/chainmail/internal/items"
)

func TestThreadKeys(t *testing.T) {
	t.Parallel()
	r := newLinkRig(t)
	initiative := r.create(t, items.NewItem{Type: items.TypeInitiative, Title: "initiative", Area: items.AreaWork})
	epic := r.create(t, items.NewItem{Type: items.TypeEpic, Title: "epic", ParentID: initiative})
	feature := r.create(t, items.NewItem{Type: items.TypeFeature, Title: "feature", ParentID: epic})
	leaf := r.create(t, items.NewItem{Type: items.TypeTask, Title: "leaf", ParentID: feature})
	chore := r.create(t, items.NewItem{Type: items.TypeTask, Title: "chore", ParentID: initiative})
	bare := r.create(t, items.NewItem{Type: items.TypeEpic, Title: "no links", ParentID: initiative})
	loner := r.task(t)
	for _, l := range []struct{ id, account, key string }{
		{initiative, "alpha", "k1"},
		{epic, "alpha", "k2"},
		{epic, "beta", "k3"},
		{leaf, "alpha", "k1"},
		{leaf, "beta", "k4"},
		{chore, "beta", "k5"},
		{loner, "alpha", "k9"},
	} {
		r.link(t, l.id, l.account, l.key)
	}
	r.remove(t, chore)
	tests := []struct {
		name string
		id   string
		want map[string][]string
	}{
		{"the whole subtree, each key once, deleted items left out", initiative, map[string][]string{"alpha": {"k1", "k2"}, "beta": {"k3", "k4"}}},
		{"a middle item and what is below it", epic, map[string][]string{"alpha": {"k1", "k2"}, "beta": {"k3", "k4"}}},
		{"a leaf", leaf, map[string][]string{"alpha": {"k1"}, "beta": {"k4"}}},
		{"another tree", loner, map[string][]string{"alpha": {"k9"}}},
		{"an item with no links", bare, map[string][]string{}},
	}
	for _, tc := range tests {
		got, err := r.store.ThreadKeys(t.Context(), tc.id)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: ThreadKeys = %v, %v; want %v", tc.name, got, err, tc.want)
		}
	}
	for _, id := range []string{chore, "it_missing"} {
		if got, err := r.store.ThreadKeys(t.Context(), id); !errors.Is(err, items.ErrNotFound) {
			t.Errorf("ThreadKeys(%s) = %v, %v; want ErrNotFound", id, got, err)
		}
	}
}

func TestLinkedThreadKeys(t *testing.T) {
	t.Parallel()
	r := newLinkRig(t)
	a, b, gone := r.task(t), r.task(t), r.task(t)
	for _, l := range []struct{ id, account, key string }{
		{a, "alpha", "k2"},
		{a, "alpha", "k1"},
		{b, "alpha", "k1"},
		{b, "beta", "k3"},
		{gone, "alpha", "k8"},
	} {
		r.link(t, l.id, l.account, l.key)
	}
	r.remove(t, gone)
	tests := []struct {
		account string
		want    []string
	}{
		{"alpha", []string{"k1", "k2"}},
		{"beta", []string{"k3"}},
		{"gamma", []string{}},
	}
	for _, tc := range tests {
		got, err := r.store.LinkedThreadKeys(t.Context(), tc.account)
		if err != nil || got == nil || !slices.Equal(got, tc.want) {
			t.Errorf("LinkedThreadKeys(%s) = %#v, %v; want %q", tc.account, got, err, tc.want)
		}
	}
}
