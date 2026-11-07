package httpapi

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
	"time"

	authnv1 "k8s.io/api/authentication/v1"
	authzv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/kuberight/kuberight/internal/config"
)

type Principal struct {
	Username string
	UID      string
	Groups   []string
	Extra    map[string]authzv1.ExtraValue
}

type AccessAttributes struct {
	Verb        string
	APIGroup    string
	Resource    string
	Subresource string
	Namespace   string
	Name        string
}

type AccessController interface {
	Authenticate(context.Context, string) (Principal, error)
	Authorize(context.Context, Principal, AccessAttributes) error
	Ready(context.Context) error
}

type accessError struct {
	status  int
	code    string
	message string
}

func (err *accessError) Error() string { return err.message }

func newAccessError(status int, code, message string) error {
	return &accessError{status: status, code: code, message: message}
}

func accessErrorDetails(err error) (int, string, string) {
	var typed *accessError
	if errors.As(err, &typed) {
		return typed.status, typed.code, typed.message
	}
	return http.StatusServiceUnavailable, "authentication_unavailable", "authentication or authorization is unavailable"
}

type staticAccess struct {
	tokenHash [sha256.Size]byte
}

func newStaticAccess(token string) (*staticAccess, error) {
	if len(token) < 32 {
		return nil, errors.New("static authentication token must contain at least 32 characters")
	}
	return &staticAccess{tokenHash: sha256.Sum256([]byte(token))}, nil
}

func (access *staticAccess) Authenticate(_ context.Context, bearer string) (Principal, error) {
	candidate := sha256.Sum256([]byte(bearer))
	if subtle.ConstantTimeCompare(candidate[:], access.tokenHash[:]) != 1 {
		return Principal{}, newAccessError(http.StatusUnauthorized, "unauthorized", "credentials were not accepted")
	}
	return Principal{Username: "static-token", Groups: []string{"kuberight:static-admin"}}, nil
}

func (*staticAccess) Authorize(_ context.Context, principal Principal, _ AccessAttributes) error {
	if principal.Username != "static-token" {
		return newAccessError(http.StatusForbidden, "forbidden", "permission was denied")
	}
	return nil
}

func (*staticAccess) Ready(context.Context) error { return nil }

type kubernetesAccess struct {
	client   kubernetes.Interface
	audience string
	timeout  time.Duration
}

func newKubernetesAccess(client kubernetes.Interface, audience string) *kubernetesAccess {
	return &kubernetesAccess{client: client, audience: audience, timeout: 5 * time.Second}
}

func (access *kubernetesAccess) Authenticate(ctx context.Context, bearer string) (Principal, error) {
	ctx, cancel := context.WithTimeout(ctx, access.timeout)
	defer cancel()
	review := &authnv1.TokenReview{Spec: authnv1.TokenReviewSpec{Token: bearer}}
	if access.audience != "" {
		review.Spec.Audiences = []string{access.audience}
	}
	result, err := access.client.AuthenticationV1().TokenReviews().Create(ctx, review, metav1.CreateOptions{})
	if err != nil {
		return Principal{}, newAccessError(http.StatusServiceUnavailable, "authentication_unavailable", "identity verification is unavailable")
	}
	if !result.Status.Authenticated || result.Status.Error != "" || strings.TrimSpace(result.Status.User.Username) == "" {
		return Principal{}, newAccessError(http.StatusUnauthorized, "unauthorized", "credentials were not accepted")
	}
	if access.audience != "" && !contains(result.Status.Audiences, access.audience) {
		return Principal{}, newAccessError(http.StatusUnauthorized, "unauthorized", "credentials were not accepted for this audience")
	}
	extra := make(map[string]authzv1.ExtraValue, len(result.Status.User.Extra))
	for key, values := range result.Status.User.Extra {
		extra[key] = append(authzv1.ExtraValue(nil), values...)
	}
	return Principal{
		Username: result.Status.User.Username,
		UID:      result.Status.User.UID,
		Groups:   append([]string(nil), result.Status.User.Groups...),
		Extra:    extra,
	}, nil
}

func (access *kubernetesAccess) Authorize(ctx context.Context, principal Principal, attributes AccessAttributes) error {
	if strings.TrimSpace(principal.Username) == "" {
		return newAccessError(http.StatusUnauthorized, "unauthorized", "an authenticated identity is required")
	}
	ctx, cancel := context.WithTimeout(ctx, access.timeout)
	defer cancel()
	review, err := access.client.AuthorizationV1().SubjectAccessReviews().Create(ctx, &authzv1.SubjectAccessReview{
		Spec: authzv1.SubjectAccessReviewSpec{
			User:   principal.Username,
			UID:    principal.UID,
			Groups: append([]string(nil), principal.Groups...),
			Extra:  principal.Extra,
			ResourceAttributes: &authzv1.ResourceAttributes{
				Namespace:   attributes.Namespace,
				Verb:        attributes.Verb,
				Group:       attributes.APIGroup,
				Resource:    attributes.Resource,
				Subresource: attributes.Subresource,
				Name:        attributes.Name,
			},
		},
	}, metav1.CreateOptions{})
	if err != nil || review.Status.EvaluationError != "" {
		return newAccessError(http.StatusServiceUnavailable, "authorization_unavailable", "permission evaluation is unavailable")
	}
	if review.Status.Denied || !review.Status.Allowed {
		return newAccessError(http.StatusForbidden, "forbidden", "permission was denied")
	}
	return nil
}

func (access *kubernetesAccess) Ready(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, access.timeout)
	defer cancel()
	discoveryClient := access.client.Discovery()
	if discoveryClient.RESTClient() == nil {
		_, err := discoveryClient.ServerVersion()
		return err
	}
	return discoveryClient.RESTClient().Get().AbsPath("/version").Do(ctx).Error()
}

func configuredAccess(cfg config.Config) (AccessController, error) {
	switch cfg.AuthMode {
	case "static":
		return newStaticAccess(cfg.APIToken)
	case "kubernetes":
		restConfig, err := rest.InClusterConfig()
		if err != nil {
			return nil, err
		}
		restConfig.QPS = cfg.KubernetesQPS
		restConfig.Burst = cfg.KubernetesBurst
		restConfig.UserAgent = "kuberight-api"
		client, err := kubernetes.NewForConfig(restConfig)
		if err != nil {
			return nil, err
		}
		return newKubernetesAccess(client, cfg.TokenAudience), nil
	default:
		return nil, errors.New("authentication mode must be explicitly configured")
	}
}

func extractBearer(header string) (string, bool) {
	parts := strings.Fields(header)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || len(parts[1]) == 0 || len(parts[1]) > 16_384 {
		return "", false
	}
	return parts[1], true
}

func contains(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}
