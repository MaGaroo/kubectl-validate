package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
	"k8s.io/apiextensions-apiserver/pkg/apiserver"
	apiextensionsschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/registry/customresource"
	"k8s.io/apiextensions-apiserver/pkg/registry/customresourcedefinition"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/kubectl-validate/pkg/openapiclient"
	"sigs.k8s.io/kubectl-validate/pkg/utils"
	"sigs.k8s.io/kubectl-validate/pkg/validatorfactory"
	"sigs.k8s.io/yaml"
)

type OutputFormat string

const (
	OutputHuman OutputFormat = "human"
	OutputJSON  OutputFormat = "json"
)

// String is used both by fmt.Print and by Cobra in help text
func (e *OutputFormat) String() string {
	return string(*e)
}

// Set must have pointer receiver so it doesn't change the value of a copy
func (e *OutputFormat) Set(v string) error {
	switch v {
	case "human", "json":
		*e = OutputFormat(v)
		return nil
	default:
		return fmt.Errorf(`must be one of "human", or "json"`)
	}
}

// Type is only used in help text
func (e *OutputFormat) Type() string {
	return "OutputFormat"
}

// A type to store list of errors for each file
type FilesErrors map[string][]error

// Returns true if there is at least a file containing a document with error
func (fe FilesErrors) hasError() bool {
	for path := range fe {
		if fe.hasFileError(path) {
			return true
		}
	}
	return false
}

// Returns true if at least a document in this file has error
func (fe FilesErrors) hasFileError(path string) bool {
	for _, err := range fe[path] {
		if err != nil {
			return true
		}
	}
	return false
}

type commandFlags struct {
	kubeConfigOverrides clientcmd.ConfigOverrides
	version             string
	localSchemasDir     string
	localCRDsDir        string
	schemaPatchesDir    string
	outputFormat        OutputFormat
}

func NewRootCommand() *cobra.Command {
	invoked := &commandFlags{
		outputFormat: OutputHuman,
		version:      "1.27",
	}
	res := &cobra.Command{
		Use:          "kubectl-validate [manifests to validate]",
		Short:        "kubectl-validate",
		Long:         "kubectl-validate is a CLI tool to validate Kubernetes manifests against their schemas",
		Args:         cobra.MinimumNArgs(1),
		RunE:         invoked.Run,
		SilenceUsage: true,
	}
	res.Flags().StringVarP(&invoked.version, "version", "", "", "Kubernetes version to validate native resources against. Required if not connected directly to cluster")
	res.Flags().StringVarP(&invoked.localSchemasDir, "local-schemas", "", "", "--local-schemas=./path/to/schemas/dir. Path to a directory with format: /apis/<group>/<version>.json for each group-version's schema.")
	res.Flags().StringVarP(&invoked.localCRDsDir, "local-crds", "", "", "--local-crds=./path/to/crds/dir. Path to a directory containing .yaml or .yml files for CRD definitions.")
	res.Flags().StringVarP(&invoked.schemaPatchesDir, "schema-patches", "", "", "Path to a directory with format: /apis/<group>/<version>.json for each group-version's schema you wish to jsonpatch to the groupversion's final schema. Patches only apply if the schema exists")
	res.Flags().VarP(&invoked.outputFormat, "output", "o", "Output format. Choice of: \"human\" or \"json\"")
	clientcmd.BindOverrideFlags(&invoked.kubeConfigOverrides, res.Flags(), clientcmd.RecommendedConfigOverrideFlags("kube-"))
	return res
}

func errorToStatus(err error) metav1.Status {
	var statusErr *k8serrors.StatusError
	var fieldError *field.Error
	var errorList utilerrors.Aggregate
	if errors.As(err, &statusErr) {
		return statusErr.ErrStatus
	} else if errors.As(err, &fieldError) {
		return k8serrors.NewInvalid(schema.GroupKind{}, "", field.ErrorList{fieldError}).ErrStatus
	} else if errors.As(err, &errorList) {
		errs := errorList.Errors()
		var fieldErrs []*field.Error
		var otherErrs []error
		for _, e := range errs {
			fieldError = nil
			if errors.As(e, &fieldError) {
				fieldErrs = append(fieldErrs, fieldError)
			} else {
				otherErrs = append(otherErrs, e)
			}
		}
		if len(otherErrs) == 0 {
			return k8serrors.NewInvalid(schema.GroupKind{}, "", fieldErrs).ErrStatus
		} else {
			return k8serrors.NewInternalError(errors.Join(otherErrs...)).ErrStatus
		}
	} else if err != nil {
		return k8serrors.NewInternalError(err).ErrStatus
	}
	return metav1.Status{Status: metav1.StatusSuccess}
}

func (c *commandFlags) Run(cmd *cobra.Command, args []string) error {
	// tool fetches openapi in the following priority order:
	factory, err := validatorfactory.New(
		openapiclient.NewOverlay(
			// apply user defined patches on top of the final schema
			openapiclient.PatchLoaderFromDirectory(nil, c.schemaPatchesDir),
			openapiclient.NewComposite(
				// consult local OpenAPI
				openapiclient.NewLocalSchemaFiles(nil, c.localSchemasDir),
				// consult local CRDs
				openapiclient.NewLocalCRDFiles(nil, c.localCRDsDir),
				openapiclient.NewOverlay(
					// apply schema extensions to builtins
					//!TODO: if kubeconfig is used, these patches may not be
					// compatible. Use active version of kubernetes to decide
					// patch to use if connected to cluster.
					openapiclient.HardcodedPatchLoader(c.version),
					// try cluster for schemas first, if they are not available
					// then fallback to hardcoded or builtin schemas
					openapiclient.NewFallback(
						// contact connected cluster for any schemas. (should this be opt-in?)
						openapiclient.NewKubeConfig(c.kubeConfigOverrides),
						// try hardcoded builtins first, if they are not available
						// fall back to GitHub builtins
						openapiclient.NewFallback(
							// schemas for known k8s versions are scraped from GH and placed here
							openapiclient.NewHardcodedBuiltins(c.version),
							// check github for builtins not hardcoded.
							// subject to rate limiting. should use a diskcache
							// since etag requests are not limited
							openapiclient.NewGitHubBuiltins(c.version),
						)),
				),
			),
		),
	)
	if err != nil {
		return err
	}

	files, err := utils.FindFiles(args...)
	if err != nil {
		return err
	}

	filesErrors := make(FilesErrors)
	for _, path := range files {
		errors := ValidateFile(path, factory)
		filesErrors[path] = errors
	}

	if c.outputFormat == OutputHuman {
		if err := printHumanErrors(filesErrors, cmd.OutOrStdout(), cmd.ErrOrStderr()); err != nil {
			return err
		}
	} else {
		if err := printJsonErrors(filesErrors, cmd.OutOrStdout(), cmd.ErrOrStderr()); err != nil {
			return err
		}
	}

	if filesErrors.hasError() {
		return errors.New("found some errors in the manifests")
	} else {
		return nil
	}
}

func ValidateFile(filePath string, resolver *validatorfactory.ValidatorFactory) []error {
	fileBytes, err := os.ReadFile(filePath)
	if err != nil {
		return []error{fmt.Errorf("error reading file: %w", err)}
	}
	if utils.IsYaml(filePath) {
		documents, err := utils.SplitYamlDocuments(fileBytes)
		if err != nil {
			return []error{err}
		}
		var errs []error
		for _, document := range documents {
			if utils.IsEmptyYamlDocument(document) {
				errs = append(errs, nil)
			} else {
				errs = append(errs, ValidateDocument(document, resolver))
			}
		}
		return errs
	} else {
		return []error{
			ValidateDocument(fileBytes, resolver),
		}
	}
}

func ValidateDocument(document []byte, resolver *validatorfactory.ValidatorFactory) error {
	metadata := metav1.TypeMeta{}
	if err := yaml.Unmarshal(document, &metadata); err != nil {
		return fmt.Errorf("failed to parse yaml: %w", err)
	}

	gvk := metadata.GetObjectKind().GroupVersionKind()
	if gvk.Empty() {
		return fmt.Errorf("GVK cannot be empty")
	}

	// CRD spec contains an infinite loop which is not supported by K8s
	// OpenAPI-based validator. Use the handwritten validation based upon
	// native types for CRD files. There are no other recursive schemas to my
	// knowledge, and any schema defined in CRD cannot be recursive.
	if gvk.Group == "apiextensions.k8s.io" && gvk.Kind == "CustomResourceDefinition" {
		obj, _, err := serializer.NewCodecFactory(apiserver.Scheme).UniversalDecoder().Decode(document, nil, nil)
		if err != nil {
			return err
		}

		strat := customresourcedefinition.NewStrategy(apiserver.Scheme)
		rest.FillObjectMetaSystemFields(obj.(metav1.Object))
		return rest.BeforeCreate(strat, request.WithNamespace(context.TODO(), ""), obj)
	}

	validators, err := resolver.ValidatorsForGVK(gvk)
	if err != nil {
		return fmt.Errorf("failed to retrieve validator: %w", err)
	}

	// Grab structural schema for use in several of the validation functions.
	// The validators use a weird mix of structural schema and openapi
	ss, err := validators.StructuralSchema()
	if err != nil || ss == nil {
		return err
	}

	// Fetch a decoder to decode this object from its structural schema
	decoder, err := validators.Decoder(gvk)
	if err != nil {
		return err
	}

	const mediaType = runtime.ContentTypeYAML
	info, ok := runtime.SerializerInfoForMediaType(decoder.SupportedMediaTypes(), mediaType)
	if !ok {
		return fmt.Errorf("unsupported media type %q", mediaType)
	}

	dec := decoder.DecoderToVersion(info.StrictSerializer, gvk.GroupVersion())
	runtimeObj, _, err := dec.Decode(document, &gvk, &unstructured.Unstructured{})
	if err != nil {
		return err
	}

	obj := runtimeObj.(*unstructured.Unstructured)

	_, err = meta.Accessor(obj)
	if err != nil {
		return field.Invalid(field.NewPath("metadata"), nil, err.Error())
	}

	isNamespaced := validators.IsNamespaceScoped()
	if isNamespaced && obj.GetNamespace() == "" {
		obj.SetNamespace("default")
	}
	if obj.GetAPIVersion() == "v1" {
		// CRD validator expects unconditoinal slashes and nonempty group,
		// since it is not originally intended for built-in
		gvk.Group = "core"
		obj.SetAPIVersion("core/v1")
	}

	strat := customresource.NewStrategy(validators.ObjectTyper(gvk), isNamespaced, gvk, validators.SchemaValidator(), nil, map[string]*apiextensionsschema.Structural{
		gvk.Version: ss,
	}, nil, nil)

	rest.FillObjectMetaSystemFields(obj)
	return rest.BeforeCreate(strat, request.WithNamespace(context.TODO(), obj.GetNamespace()), obj)
}

func printHumanErrors(filesErrors FilesErrors, outWriter io.Writer, errWriter io.Writer) error {
	for path, errs := range filesErrors {
		fmt.Fprintf(outWriter, "\n\033[1m%v\033[0m...", path)
		if filesErrors.hasFileError(path) {
			fmt.Fprintln(outWriter, "\033[31mERROR\033[0m")
			for _, err := range errs {
				if err != nil {
					fmt.Fprintln(errWriter, err.Error())
				}
			}
		} else {
			fmt.Fprintln(outWriter, "\033[32mOK\033[0m")
		}
	}
	return nil
}

func printJsonErrors(filesErrors FilesErrors, outWriter io.Writer, errWriter io.Writer) error {
	res := map[string][]metav1.Status{}
	for path, errs := range filesErrors {
		for _, err := range errs {
			res[path] = append(res[path], errorToStatus(err))
		}
	}
	data, e := json.MarshalIndent(res, "", "    ")
	if e != nil {
		return fmt.Errorf("failed to render results into JSON: %w", e)
	}
	fmt.Fprintln(outWriter, string(data))
	return nil
}
