package verifier

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/nirmata/kyverno-notation-verifier/pkg/notationfactory"
	"github.com/nirmata/kyverno-notation-verifier/types"
	"github.com/notaryproject/notation-go"
	notationlog "github.com/notaryproject/notation-go/log"
	notationregistry "github.com/notaryproject/notation-go/registry"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"go.uber.org/multierr"
	"go.uber.org/zap"
	kubeinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"oras.land/oras-go/v2/registry"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	ctrl "sigs.k8s.io/controller-runtime"
)

func newVerifier(logger *zap.SugaredLogger, opts ...verifierOptsFunc) (*verifier, error) {
	v := &verifier{
		logger: logger,
	}

	config, err := ctrl.GetConfig()
	if err != nil {
		return nil, errors.Wrap(err, "failed to get Kubernetes config")
	}

	v.kubeClient, err = kubernetes.NewForConfig(config)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create Kubernetes client")
	}

	v.notationVerifierFactory = notationfactory.NewNotationVerifierFactory(logger)
	err = v.notationVerifierFactory.RefreshVerifiers()
	if err != nil {
		v.logger.Errorf("failed to create notation verifiers, error: %v", err)
		return nil, err
	}
	v.logger.Info("notation verifier created")

	namespace := os.Getenv("POD_NAMESPACE")
	v.informerFactory = kubeinformers.NewSharedInformerFactoryWithOptions(v.kubeClient, 15*time.Minute, kubeinformers.WithNamespace(namespace))
	v.secretLister = v.informerFactory.Core().V1().Secrets().Lister().Secrets(namespace)
	v.configMapLister = v.informerFactory.Core().V1().ConfigMaps().Lister().ConfigMaps(namespace)

	for _, o := range opts {
		o(v)
	}

	v.logger.Infow("initialized", "namespace", namespace, "secrets", v.imagePullSecrets,
		"insecureRegistry", v.insecureRegistry)

	v.stopCh = make(chan struct{})
	go v.informerFactory.Start(v.stopCh)

	return v, nil
}

func (v *verifier) verifyImages(ctx context.Context, requestData *types.RequestData) ([]byte, error) {
	verificationFailed := false
	images := requestData.Images

	response := types.ResponseData{
		Verified: true,
		Results:  make([]types.Result, 0),
	}

	notationVerifier, err := v.notationVerifierFactory.GetVerifier(requestData)
	if err != nil {
		verificationFailed = true
		response.Verified = false
		response.ErrorMessage = fmt.Sprintf("failed to create notation verifier: %s", err.Error())
		v.logger.Errorf("failed to create notation verifier: %s", err.Error())
	}

	if !verificationFailed {
		for _, image := range images.Containers {
			result, err := v.verifyImageInfo(ctx, notationVerifier, &image)
			if err != nil {
				verificationFailed = true
				response.Verified = false
				response.ErrorMessage = fmt.Sprintf("failed to verify container %s: %s", image.Name, err.Error())
				v.logger.Errorf("failed to verify container %s: %s", image.Name, err.Error())
				break
			}
			v.logger.Infof("Verified image %s: %s", image.String(), result.Image)
			response.Results = append(response.Results, *result)
		}
		v.logger.Infof("verified %d containers ", images.Containers)
	}

	if !verificationFailed {
		for _, image := range images.InitContainers {
			result, err := v.verifyImageInfo(ctx, notationVerifier, &image)
			if err != nil {
				verificationFailed = true
				response.Verified = false
				response.ErrorMessage = fmt.Sprintf("failed to verify init container %s: %s", image.Name, err.Error())
				v.logger.Errorf("failed to verify container %s: %s", image.Name, err.Error())
				break
			}
			v.logger.Infof("Verified image %s: %s", image.String(), result.Image)
			response.Results = append(response.Results, *result)
		}
		v.logger.Infof("verified %d initContainers", images.InitContainers)
	}

	if !verificationFailed {
		for _, image := range images.EphemeralContainers {
			result, err := v.verifyImageInfo(ctx, notationVerifier, &image)
			if err != nil {
				verificationFailed = true
				response.Verified = false
				response.ErrorMessage = fmt.Sprintf("failed to verify ephemeral container: %s: %s", image.Name, err.Error())
				v.logger.Errorf("failed to verify container %s: %s", image.Name, err.Error())
				break
			}
			v.logger.Infof("Verified image %s: %s", image.String(), result.Image)
			response.Results = append(response.Results, *result)
		}
		v.logger.Infof("verified %d ephemeralContainers", images.EphemeralContainers)
	}

	if verificationFailed {
		response.Results = nil
	}

	data, err := json.MarshalIndent(response, "  ", "  ")
	if err != nil {
		return nil, errors.Wrapf(err, "failed to marshal response")
	}

	return data, nil
}

func (v *verifier) verifyImageInfo(ctx context.Context, notationVerifier *notation.Verifier, image *types.ImageInfo) (*types.Result, error) {
	v.logger.Infof("verifying image infos %+v", image)
	digest, err := v.verifyImage(ctx, notationVerifier, image.String())
	if err != nil {
		v.logger.Errorf("verification failed for image %s: %v", image, err)
		return nil, errors.Wrapf(err, "failed to verify image %s", image)
	}

	image.Digest = digest

	return &types.Result{
		Name:  image.Name,
		Path:  image.Pointer,
		Image: image.String(),
	}, nil
}

func (v *verifier) verifyImage(ctx context.Context, notationVerifier *notation.Verifier, image string) (string, error) {
	v.logger.Infof("verifying image %s", image)
	repo, reference, err := v.parseReferenceAndResolveDigest(ctx, image)
	if err != nil {
		return "", errors.Wrapf(err, "failed to resolve digest")
	}

	pluginConfig := map[string]string{}
	if v.pluginConfigMap != "" {
		cm, err := v.configMapLister.Get(v.pluginConfigMap)
		if err != nil {
			return "", errors.Wrapf(err, "failed to fetch plugin configmap %s", v.pluginConfigMap)
		}

		for k, v := range cm.Data {
			pluginConfig[k] = v
		}
	}

	opts := notation.VerifyOptions{
		ArtifactReference:    reference.String(),
		MaxSignatureAttempts: v.maxSignatureAttempts,
		PluginConfig:         pluginConfig,
	}

	nlog := notationlog.WithLogger(ctx, notationlog.Discard)
	if v.debug {
		pluginConfig["debug"] = "true"
		nlog = notationlog.WithLogger(ctx, v.logger)
	}

	desc, outcomes, err := notation.Verify(nlog, *notationVerifier, repo, opts)
	if err != nil {
		return "", err
	}

	var errs []error
	for _, o := range outcomes {
		if o.Error != nil {
			errs = append(errs, o.Error)
		}
	}

	if len(errs) > 0 {
		err := multierr.Combine(errs...)
		return "", err
	}

	return desc.Digest.String(), nil
}

func (v *verifier) parseReferenceAndResolveDigest(ctx context.Context, ref string) (notationregistry.Repository, registry.Reference, error) {
	if !strings.Contains(ref, "/") {
		ref = "docker.io/library/" + ref
	}

	if !strings.Contains(ref, ":") {
		ref = ref + ":latest"
	}

	parsedRef, err := registry.ParseReference(ref)
	if err != nil {
		return nil, registry.Reference{}, errors.Wrapf(err, "failed to parse reference %s", ref)
	}

	authClient, plainHTTP, err := v.getAuthClient(ctx, parsedRef)
	if err != nil {
		return nil, registry.Reference{}, errors.Wrapf(err, "failed to retrieve credentials")
	}

	repo, err := remote.NewRepository(ref)
	if err != nil {
		return nil, registry.Reference{}, errors.Wrapf(err, "failed to initialize repository")
	}

	repo.PlainHTTP = plainHTTP
	repo.Client = authClient
	repository := notationregistry.NewRepository(repo)

	parsedRef, err = v.resolveDigest(repository, parsedRef)
	if err != nil {
		return nil, registry.Reference{}, errors.Wrapf(err, "failed to resolve digest")
	}

	return repository, parsedRef, nil
}

func (v *verifier) getAuthClient(ctx context.Context, ref registry.Reference) (*auth.Client, bool, error) {
	authConfig, err := v.getAuthConfig(ctx, ref)
	if err != nil {
		return nil, false, err
	}

	credentials := auth.Credential{
		Username:     authConfig.Username,
		Password:     authConfig.Password,
		AccessToken:  authConfig.IdentityToken,
		RefreshToken: authConfig.RegistryToken,
	}

	authClient := &auth.Client{
		Credential: func(ctx context.Context, registry string) (auth.Credential, error) {
			switch registry {
			default:
				return credentials, nil
			}
		},
		Cache:    auth.NewCache(),
		ClientID: "notation",
	}

	authClient.SetUserAgent("kyverno.io")
	return authClient, false, nil
}

func (v *verifier) resolveDigest(repo notationregistry.Repository, ref registry.Reference) (registry.Reference, error) {
	if isDigestReference(ref.String()) {
		return ref, nil
	}

	// Resolve tag reference to digest reference.
	manifestDesc, err := v.getManifestDescriptorFromReference(repo, ref.String())
	if err != nil {
		return registry.Reference{}, err
	}

	ref.Reference = manifestDesc.Digest.String()
	return ref, nil
}

func isDigestReference(reference string) bool {
	parts := strings.SplitN(reference, "/", 2)
	if len(parts) == 1 {
		return false
	}

	index := strings.Index(parts[1], "@")
	return index != -1
}

func (v *verifier) getManifestDescriptorFromReference(repo notationregistry.Repository, reference string) (ocispec.Descriptor, error) {
	ref, err := registry.ParseReference(reference)
	if err != nil {
		return ocispec.Descriptor{}, err
	}

	return repo.Resolve(context.Background(), ref.ReferenceOrDefault())
}
