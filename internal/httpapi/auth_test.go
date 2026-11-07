package httpapi

import (
	"context"
	"errors"
	"net/http"
	"testing"

	authnv1 "k8s.io/api/authentication/v1"
	authzv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

func TestStaticAccessFailsClosed(t *testing.T) {
	access, err := newStaticAccess("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := access.Authenticate(context.Background(), "wrong"); err == nil {
		t.Fatal("invalid token was authenticated")
	}
	principal, err := access.Authenticate(context.Background(), "0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatalf("valid token was rejected: %v", err)
	}
	if err := access.Authorize(context.Background(), principal, AccessAttributes{}); err != nil {
		t.Fatalf("static principal was rejected: %v", err)
	}
}

func TestKubernetesAccessUsesTokenReviewAndNamespaceSAR(t *testing.T) {
	client := fake.NewSimpleClientset()
	client.PrependReactor("create", "tokenreviews", func(action clienttesting.Action) (bool, runtime.Object, error) {
		review := action.(clienttesting.CreateAction).GetObject().(*authnv1.TokenReview)
		if review.Spec.Token != "valid" {
			t.Fatal("unexpected bearer token")
		}
		return true, &authnv1.TokenReview{Status: authnv1.TokenReviewStatus{
			Authenticated: true,
			User:          authnv1.UserInfo{Username: "alice", UID: "123", Groups: []string{"developers"}},
		}}, nil
	})
	client.PrependReactor("create", "subjectaccessreviews", func(action clienttesting.Action) (bool, runtime.Object, error) {
		review := action.(clienttesting.CreateAction).GetObject().(*authzv1.SubjectAccessReview)
		attributes := review.Spec.ResourceAttributes
		if review.Spec.User != "alice" || attributes.Namespace != "payments" || attributes.Resource != "recommendations" || attributes.Verb != "create" {
			t.Fatalf("incorrect access review: %#v", review.Spec)
		}
		return true, &authzv1.SubjectAccessReview{Status: authzv1.SubjectAccessReviewStatus{Allowed: true}}, nil
	})

	access := newKubernetesAccess(client, "")
	principal, err := access.Authenticate(context.Background(), "valid")
	if err != nil {
		t.Fatal(err)
	}
	if err := access.Authorize(context.Background(), principal, AccessAttributes{Verb: "create", APIGroup: "kuberight.io", Resource: "recommendations", Namespace: "payments"}); err != nil {
		t.Fatal(err)
	}
}

func TestKubernetesAccessDeniesFailedAndUnavailableReviews(t *testing.T) {
	client := fake.NewSimpleClientset()
	client.PrependReactor("create", "tokenreviews", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("api unavailable")
	})
	access := newKubernetesAccess(client, "")
	_, err := access.Authenticate(context.Background(), "valid")
	status, code, _ := accessErrorDetails(err)
	if status != http.StatusServiceUnavailable || code != "authentication_unavailable" {
		t.Fatalf("unexpected API failure result: status=%d code=%q err=%v", status, code, err)
	}

	client = fake.NewSimpleClientset()
	client.PrependReactor("create", "subjectaccessreviews", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, &authzv1.SubjectAccessReview{Status: authzv1.SubjectAccessReviewStatus{Allowed: false}}, nil
	})
	access = newKubernetesAccess(client, "")
	err = access.Authorize(context.Background(), Principal{Username: "alice"}, AccessAttributes{Verb: "get", Resource: "recommendations"})
	status, code, _ = accessErrorDetails(err)
	if status != http.StatusForbidden || code != "forbidden" {
		t.Fatalf("unexpected denial result: status=%d code=%q err=%v", status, code, err)
	}
}

func TestKubernetesAccessRejectsWrongAudience(t *testing.T) {
	client := fake.NewSimpleClientset()
	client.PrependReactor("create", "tokenreviews", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, &authnv1.TokenReview{Status: authnv1.TokenReviewStatus{
			Authenticated: true,
			Audiences:     []string{"another-service"},
			User:          authnv1.UserInfo{Username: "alice"},
		}}, nil
	})
	access := newKubernetesAccess(client, "kuberight")
	if _, err := access.Authenticate(context.Background(), "valid"); err == nil {
		t.Fatal("token for the wrong audience was accepted")
	}
}

// Ensure fake discovery has a stable GroupVersion in tests that call it.
var _ = metav1.APIGroup{PreferredVersion: metav1.GroupVersionForDiscovery{GroupVersion: schema.GroupVersion{}.String()}}
