package imapstore

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/yaklabco/chainmail/internal/auth"
	"github.com/yaklabco/chainmail/internal/mailstore"
	"github.com/yaklabco/chainmail/internal/ref"
)

func TestThreadOf(t *testing.T) {
	t.Parallel()
	seed := ref.Ref{Account: "a", Folder: "INBOX", UIDValidity: 1, UID: 1}
	inbox := []ref.Ref{seed, {Account: "a", Folder: "INBOX", UIDValidity: 1, UID: 2}}
	sent := []ref.Ref{{Account: "a", Folder: "Sent", UIDValidity: 1, UID: 9}}
	unavailable := mailstore.ErrUnavailable
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	for name, tc := range map[string]struct {
		ctx     context.Context
		found   [][]ref.Ref
		errs    []error
		want    []ref.Ref
		wantErr error
	}{
		"all folders answer":   {found: [][]ref.Ref{inbox, sent}, errs: []error{nil, nil}, want: append(slices.Clone(inbox), sent...)},
		"one folder fails":     {found: [][]ref.Ref{inbox, nil}, errs: []error{nil, unavailable}, want: inbox},
		"every folder fails":   {found: [][]ref.Ref{nil, nil}, errs: []error{unavailable, unavailable}, wantErr: unavailable},
		"a login refusal":      {found: [][]ref.Ref{inbox, nil}, errs: []error{nil, &auth.RejectedError{Err: auth.ErrRejected}}, wantErr: auth.ErrRejected},
		"the context ended":    {ctx: cancelled, found: [][]ref.Ref{inbox, nil}, errs: []error{nil, context.Canceled}, wantErr: context.Canceled},
		"the seed is not seen": {found: [][]ref.Ref{sent}, errs: []error{nil}, want: append(slices.Clone(sent), seed)},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := tc.ctx
			if ctx == nil {
				ctx = t.Context()
			}
			got, err := threadOf(ctx, tc.found, tc.errs, seed)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) || got != nil {
					t.Errorf("threadOf = %v, %v; want %v", got, err, tc.wantErr)
				}
				return
			}
			if err != nil || !slices.Equal(got, tc.want) {
				t.Errorf("threadOf = %v, %v; want %v", got, err, tc.want)
			}
		})
	}
}
