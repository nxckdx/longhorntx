package overview

import (
	"errors"
	"fmt"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestNotInstalled(t *testing.T) {
	nf := apierrors.NewNotFound(schema.GroupResource{Group: "longhorn.io", Resource: "volumes"}, "")
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{nf, true},
		{fmt.Errorf("list volumes: %w", nf), true},
		{errors.New("the server could not find the requested resource"), true},
		{errors.New("dial tcp 10.0.0.1:6443: connect: connection refused"), false},
		{apierrors.NewForbidden(schema.GroupResource{Group: "longhorn.io", Resource: "volumes"}, "", errors.New("no")), false},
	} {
		if got := NotInstalled(tc.err); got != tc.want {
			t.Errorf("%v: got %v", tc.err, got)
		}
	}
}
