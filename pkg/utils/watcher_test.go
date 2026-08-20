package utils

import (
	"errors"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestWatchErrorPreservesStatusError(t *testing.T) {
	status := &metav1.Status{
		Status:  metav1.StatusFailure,
		Reason:  metav1.StatusReasonForbidden,
		Message: "endpointslices is forbidden",
	}

	err := WatchError(status)
	var statusErr *apierrors.StatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("expected a status error, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), status.Message) {
		t.Fatalf("expected error to contain %q, got %q", status.Message, err)
	}
}

func TestWatchErrorHandlesUnexpectedObject(t *testing.T) {
	err := WatchError(&metav1.APIGroup{})
	if err == nil {
		t.Fatal("expected an error for an unexpected watch object")
	}
	if !strings.Contains(err.Error(), "unknown watch error object") {
		t.Fatalf("expected unexpected-object context, got %q", err)
	}
}
