package importer

import (
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/kubernetes/pkg/api/legacyscheme"
	kapi "k8s.io/kubernetes/pkg/apis/core"
	"k8s.io/kubernetes/pkg/kubectl"
	"k8s.io/kubernetes/pkg/kubectl/cmd/templates"
	kcmdutil "k8s.io/kubernetes/pkg/kubectl/cmd/util"
	"k8s.io/kubernetes/pkg/kubectl/genericclioptions"
	oldresource "k8s.io/kubernetes/pkg/kubectl/resource"
	"k8s.io/kubernetes/pkg/printers"

	configcmd "github.com/openshift/origin/pkg/bulk"
	"github.com/openshift/origin/pkg/oc/cli/util/clientcmd"
	"github.com/openshift/origin/pkg/oc/generate/app"
	"github.com/openshift/origin/pkg/oc/generate/appjson"
	appcmd "github.com/openshift/origin/pkg/oc/generate/cmd"
	templateinternalclient "github.com/openshift/origin/pkg/template/client/internalversion"
	templateclientinternal "github.com/openshift/origin/pkg/template/generated/internalclientset"
	templateclient "github.com/openshift/origin/pkg/template/generated/internalclientset/typed/template/internalversion"
)

const AppJSONV1GeneratorName = "app-json/v1"

var (
	appJSONLong = templates.LongDesc(`
		Import app.json files as OpenShift objects

		app.json defines the pattern of a simple, stateless web application that can be horizontally scaled.
		This command will transform a provided app.json object into its OpenShift equivalent.
		During transformation fields in the app.json syntax that are not relevant when running on top of
		a containerized platform will be ignored and a warning printed.

		The command will create objects unless you pass the -o yaml or --as-template flags to generate a
		configuration file for later use.

		Experimental: This command is under active development and may change without notice.`)

	appJSONExample = templates.Examples(`
		# Import a directory containing an app.json file
	  $ %[1]s app.json -f .

	  # Turn an app.json file into a template
	  $ %[1]s app.json -f ./app.json -o yaml --as-template`)
)

type AppJSONOptions struct {
	genericclioptions.IOStreams
	oldresource.FilenameOptions
	PrintFlags *genericclioptions.PrintFlags

	PrintObj printers.ResourcePrinterFunc

	Action configcmd.BulkAction

	BaseImage  string
	Generator  string
	AsTemplate string

	OutputVersions []schema.GroupVersion

	Namespace string
	Client    templateclient.TemplateInterface
}

func NewAppJSONOptions(streams genericclioptions.IOStreams) *AppJSONOptions {
	return &AppJSONOptions{
		Action: configcmd.BulkAction{
			Out:    streams.Out,
			ErrOut: streams.ErrOut,
		},
		IOStreams:  streams,
		PrintFlags: genericclioptions.NewPrintFlags(""),
		Generator:  AppJSONV1GeneratorName,
	}
}

// NewCmdAppJSON imports an app.json file (schema described here: https://devcenter.heroku.com/articles/app-json-schema)
// as a template.
func NewCmdAppJSON(fullName string, f *clientcmd.Factory, streams genericclioptions.IOStreams) *cobra.Command {
	o := NewAppJSONOptions(streams)
	cmd := &cobra.Command{
		Use:     "app.json -f APPJSON",
		Short:   "Import an app.json definition into OpenShift (experimental)",
		Long:    appJSONLong,
		Example: fmt.Sprintf(appJSONExample, fullName),
		Run: func(cmd *cobra.Command, args []string) {
			kcmdutil.CheckErr(o.Complete(f, cmd, args))
			kcmdutil.CheckErr(o.Validate())
			if err := o.Run(); err != nil {
				// TODO: move me to kcmdutil
				if err == kcmdutil.ErrExit {
					os.Exit(1)
				}
				kcmdutil.CheckErr(err)
			}
		},
	}
	usage := "Filename, directory, or URL to app.json file to use"
	kubectl.AddJsonFilenameFlag(cmd, &o.Filenames, usage)
	cmd.MarkFlagRequired("filename")

	cmd.Flags().StringVar(&o.BaseImage, "image", o.BaseImage, "An optional image to use as your base Docker build (must have ONBUILD directives)")
	cmd.Flags().String("generator", o.Generator, "The name of the generator strategy to use - specify this value to for backwards compatibility.")
	cmd.Flags().StringVar(&o.AsTemplate, "as-template", "", "If set, generate a template with the provided name")
	cmd.Flags().String("output-version", "", "The preferred API versions of the output objects")

	o.Action.BindForOutput(cmd.Flags())
	o.PrintFlags.AddFlags(cmd)
	return cmd
}

func (o *AppJSONOptions) Complete(f *clientcmd.Factory, cmd *cobra.Command, args []string) error {
	version, _ := cmd.Flags().GetString("output-version")
	for _, v := range strings.Split(version, ",") {
		gv, err := schema.ParseGroupVersion(v)
		if err != nil {
			return fmt.Errorf("provided output-version %q is not valid: %v", v, err)
		}
		o.OutputVersions = append(o.OutputVersions, gv)
	}
	o.OutputVersions = append(o.OutputVersions, legacyscheme.Registry.EnabledVersions()...)

	o.Action.Bulk.Mapper = clientcmd.ResourceMapper(f)
	o.Action.Bulk.Op = configcmd.Create

	printer, err := o.PrintFlags.ToPrinter()
	if err != nil {
		return err
	}
	o.PrintObj = printer.PrintObj

	o.Generator, _ = cmd.Flags().GetString("generator")

	ns, _, err := f.DefaultNamespace()
	if err != nil {
		return err
	}
	o.Namespace = ns

	clientConfig, err := f.ClientConfig()
	if err != nil {
		return err
	}
	templateClient, err := templateclientinternal.NewForConfig(clientConfig)
	if err != nil {
		return err
	}
	o.Client = templateClient.Template()

	return nil
}

func (o *AppJSONOptions) Validate() error {
	if len(o.Filenames) != 1 {
		return fmt.Errorf("you must provide the path to an app.json file or directory containing app.json")
	}
	switch o.Generator {
	case AppJSONV1GeneratorName:
	default:
		return fmt.Errorf("the generator %q is not supported, use: %s", o.Generator, AppJSONV1GeneratorName)
	}
	return nil
}

func (o *AppJSONOptions) Run() error {
	localPath, contents, err := contentsForPathOrURL(o.Filenames[0], o.In, "app.json")
	if err != nil {
		return err
	}

	g := &appjson.Generator{
		LocalPath: localPath,
		BaseImage: o.BaseImage,
	}
	switch {
	case len(o.AsTemplate) > 0:
		g.Name = o.AsTemplate
	case len(localPath) > 0:
		g.Name = filepath.Base(localPath)
	default:
		g.Name = path.Base(path.Dir(o.Filenames[0]))
	}
	if len(g.Name) == 0 {
		g.Name = "app"
	}

	template, err := g.Generate(contents)
	if err != nil {
		return err
	}

	template.ObjectLabels = map[string]string{"app.json": template.Name}

	// all the types generated into the template should be known
	if errs := app.AsVersionedObjects(template.Objects, legacyscheme.Scheme, legacyscheme.Scheme, o.OutputVersions...); len(errs) > 0 {
		for _, err := range errs {
			fmt.Fprintf(o.Action.ErrOut, "error: %v\n", err)
		}
	}

	if o.Action.ShouldPrint() || (o.Action.Output == "name" && len(o.AsTemplate) > 0) {
		var obj runtime.Object
		if len(o.AsTemplate) > 0 {
			template.Name = o.AsTemplate
			obj = template
		} else {
			obj = &kapi.List{Items: template.Objects}
		}
		return o.PrintObj(obj, o.Out)
	}

	templateProcessor := templateinternalclient.NewTemplateProcessorClient(o.Client.RESTClient(), o.Namespace)
	result, err := appcmd.TransformTemplate(template, templateProcessor, o.Namespace, nil, false)
	if err != nil {
		return err
	}

	if o.Action.Verbose() {
		appcmd.DescribeGeneratedTemplate(o.Action.Out, "", result, o.Namespace)
	}

	if errs := o.Action.WithMessage("Importing app.json", "creating").Run(&kapi.List{Items: result.Objects}, o.Namespace); len(errs) > 0 {
		return kcmdutil.ErrExit
	}
	return nil
}

func contentsForPathOrURL(s string, in io.Reader, subpaths ...string) (string, []byte, error) {
	switch {
	case s == "-":
		contents, err := ioutil.ReadAll(in)
		return "", contents, err
	case strings.Index(s, "http://") == 0 || strings.Index(s, "https://") == 0:
		_, err := url.Parse(s)
		if err != nil {
			return "", nil, fmt.Errorf("the URL passed to filename %q is not valid: %v", s, err)
		}
		res, err := http.Get(s)
		if err != nil {
			return "", nil, err
		}
		defer res.Body.Close()
		contents, err := ioutil.ReadAll(res.Body)
		return "", contents, err
	default:
		stat, err := os.Stat(s)
		if err != nil {
			return s, nil, err
		}
		if !stat.IsDir() {
			contents, err := ioutil.ReadFile(s)
			return s, contents, err
		}
		for _, sub := range subpaths {
			path := filepath.Join(s, sub)
			stat, err := os.Stat(path)
			if err != nil {
				continue
			}
			if stat.IsDir() {
				continue
			}
			contents, err := ioutil.ReadFile(s)
			return path, contents, err
		}
		return s, nil, os.ErrNotExist
	}
}
