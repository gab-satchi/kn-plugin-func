package cmd

import (
	"errors"
	"fmt"

	"github.com/AlecAivazis/survey/v2"
	"github.com/AlecAivazis/survey/v2/terminal"
	"github.com/ory/viper"
	"github.com/spf13/cobra"

	"knative.dev/kn-plugin-func/buildpacks"
	"knative.dev/kn-plugin-func/s2i"

	fn "knative.dev/kn-plugin-func"
	"knative.dev/kn-plugin-func/builders"
)

func NewBuildCmd(newClient ClientFactory) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "build",
		Short: "Build a function project as a container image",
		Long: `Build a function project as a container image

This command builds the function project in the current directory or in the directory
specified by --path. The result will be a container image that is pushed to a registry.
The func.yaml file is read to determine the image name and registry.
If the project has not already been built, either --registry or --image must be provided
and the image name is stored in the configuration file.
`,
		Example: `
# Build from the local directory, using the given registry as target.
# The full image name will be determined automatically based on the
# project directory name
{{.Name}} build --registry quay.io/myuser

# Build from the local directory, specifying the full image name
{{.Name}} build --image quay.io/myuser/myfunc

# Re-build, picking up a previously supplied image name from a local func.yml
{{.Name}} build

# Build using s2i instead of Buildpacks
{{.Name}} build --builder=s2i

# Build with a custom buildpack builder
{{.Name}} build --builder=pack --builder-image cnbs/sample-builder:bionic
`,
		SuggestFor: []string{"biuld", "buidl", "built"},
		PreRunE:    bindEnv("image", "path", "builder", "registry", "confirm", "push", "builder-image", "platform"),
	}

	cmd.Flags().StringP("builder", "b", builders.Default, fmt.Sprintf("build strategy to use when creating the underlying image. Currently supported build strategies are %s.", KnownBuilders()))
	cmd.Flags().StringP("builder-image", "", "", "builder image, either an as a an image name or a mapping name.\nSpecified value is stored in func.yaml (as 'builder' field) for subsequent builds. ($FUNC_BUILDER_IMAGE)")
	cmd.Flags().BoolP("confirm", "c", false, "Prompt to confirm all configuration options (Env: $FUNC_CONFIRM)")
	cmd.Flags().StringP("image", "i", "", "Full image name in the form [registry]/[namespace]/[name]:[tag] (optional). This option takes precedence over --registry (Env: $FUNC_IMAGE)")
	cmd.Flags().StringP("registry", "r", GetDefaultRegistry(), "Registry + namespace part of the image to build, ex 'quay.io/myuser'.  The full image name is automatically determined based on the local directory name. If not provided the registry will be taken from func.yaml (Env: $FUNC_REGISTRY)")
	cmd.Flags().BoolP("push", "u", false, "Attempt to push the function image after being successfully built")
	cmd.Flags().StringP("platform", "", "", "Target platform to build (e.g. linux/amd64).")
	setPathFlag(cmd)

	if err := cmd.RegisterFlagCompletionFunc("builder", CompleteBuildersList); err != nil {
		fmt.Println("internal: error while calling RegisterFlagCompletionFunc: ", err)
	}

	if err := cmd.RegisterFlagCompletionFunc("builder-image", CompleteBuilderImageList); err != nil {
		fmt.Println("internal: error while calling RegisterFlagCompletionFunc: ", err)
	}

	cmd.SetHelpFunc(defaultTemplatedHelp)

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		return runBuild(cmd, args, newClient)
	}

	return cmd
}

// ValidateBuilder ensures that the given builder is one that the CLI
// knows how to instantiate, returning a builkder.ErrUnknownBuilder otherwise.
func ValidateBuilder(name string) (err error) {
	for _, known := range KnownBuilders() {
		if name == known {
			return
		}
	}
	return builders.ErrUnknownBuilder{Name: name, Known: KnownBuilders()}
}

func ValidNamespaceAndRegistry(path string) survey.Validator {
	return func(val interface{}) error {

		// if the value passed in is the zero value of the appropriate type
		if len(val.(string)) == 0 {
			return errors.New("Value is required")
		}

		_, err := fn.DerivedImage(path, val.(string)) //image can be derived without any error

		if err != nil {
			return fmt.Errorf("invalid registry [%q]: %w", val.(string), err)
		}
		return nil
	}
}

// KnownBuilders are a typed string slice of builder short names which this
// CLI understands.  Includes a customized String() representation intended
// for use in flags and help text.
func KnownBuilders() builders.Known {
	// The set of builders supported by this CLI will likely always equate to
	// the set of builders enumerated in the builders pacakage.
	// However, future third-party integrations may support less than, or more
	// builders, and certain environmental considerations may alter this list.
	return builders.All()
}

func runBuild(cmd *cobra.Command, _ []string, newClient ClientFactory) (err error) {
	config, err := newBuildConfig().Prompt()
	if err != nil {
		if errors.Is(err, terminal.InterruptErr) {
			return nil
		}
		return
	}

	function, err := functionWithOverrides(config.Path, functionOverrides{Image: config.Image})
	if err != nil {
		return
	}

	// Check if the function has been initialized
	if !function.Initialized() {
		return fmt.Errorf("'%v' does not contain an initialized function", config.Path)
	}

	// If a registry name was provided as a command line flag, it should be validated
	if config.Registry != "" {
		err = ValidNamespaceAndRegistry(config.Path)(config.Registry)
		if err != nil {
			return
		}
	} else if function.Registry != "" {
		// Otherwise, if a registry name was provided in func.config, set that value
		// for the command configuration and validate it
		config.Registry = function.Registry
		err = ValidNamespaceAndRegistry(config.Path)(config.Registry)
		if err != nil {
			return
		}
	}

	// If the function does not yet have an image name and one was not provided on the command line
	if function.Image == "" {
		//  AND a --registry was not provided, then we need to
		// prompt for a registry from which we can derive an image name.
		if config.Registry == "" {
			fmt.Println("A registry for function images is required. For example, 'docker.io/tigerteam'.")

			err = survey.AskOne(
				&survey.Input{Message: "Registry for function images:"},
				&config.Registry, survey.WithValidator(ValidNamespaceAndRegistry(config.Path)))
			if err != nil {
				if errors.Is(err, terminal.InterruptErr) {
					return nil
				}
				return
			}
			fmt.Println("Note: building a function the first time will take longer than subsequent builds")
		}

		// We have the registry, so let's use it to derive the function image name
		config.Image = deriveImage(config.Image, config.Registry, config.Path)
		function.Image = config.Image
	}

	// Choose a builder based on the value of the --builder flag
	var builder fn.Builder
	if function.Builder == "" || cmd.Flags().Changed("builder") {
		function.Builder = config.Builder
	} else {
		config.Builder = function.Builder
	}
	if err = ValidateBuilder(config.Builder); err != nil {
		return err
	}
	if config.Builder == builders.Pack {
		if config.Platform != "" {
			err = fmt.Errorf("the --platform flag works only with s2i build")
			return
		}
		builder = buildpacks.NewBuilder(
			buildpacks.WithName(builders.Pack),
			buildpacks.WithVerbose(config.Verbose))
	} else if config.Builder == builders.S2I {
		builder = s2i.NewBuilder(
			s2i.WithName(builders.S2I),
			s2i.WithVerbose(config.Verbose),
			s2i.WithPlatform(config.Platform))
	}

	// All set, let's write changes in the config to the disk
	err = function.Write()
	if err != nil {
		return
	}

	// Use the user-provided builder image, if supplied
	if config.BuilderImage != "" {
		function.BuilderImages[config.Builder] = config.BuilderImage
	}

	// Create a client using the registry defined in config plus any additional
	// options provided (such as mocks for testing)
	client, done := newClient(ClientConfig{Verbose: config.Verbose},
		fn.WithRegistry(config.Registry),
		fn.WithBuilder(builder))
	defer done()

	err = client.Build(cmd.Context(), config.Path)
	if err == nil && config.Push {
		err = client.Push(cmd.Context(), config.Path)
	}
	return
}

type buildConfig struct {
	// Image name in full, including registry, repo and tag (overrides
	// image name derivation based on registry and function name)
	Image string

	// Path of the function implementation on local disk. Defaults to current
	// working directory of the process.
	Path string

	// Push the resulting image to the registry after building.
	Push bool

	// Registry at which interstitial build artifacts should be kept.
	// This setting is ignored if Image is specified, which includes the full
	Registry string

	// Verbose logging.
	Verbose bool

	// Confirm: confirm values arrived upon from environment plus flags plus defaults,
	// with interactive prompting (only applicable when attached to a TTY).
	Confirm bool

	// Builder is the name of the subsystem that will complete the underlying
	// build (Pack, s2i, remote pipeline, etc).  Currently ad-hoc rather than
	// an enumerated field.  See the Client constructory for logic.
	Builder string

	// BuilderImage is the image (name or mapping) to use for building.  Usually
	// set automatically.
	BuilderImage string

	Platform string
}

func newBuildConfig() buildConfig {
	return buildConfig{
		Image:        viper.GetString("image"),
		Path:         getPathFlag(),
		Registry:     viper.GetString("registry"),
		Verbose:      viper.GetBool("verbose"), // defined on root
		Confirm:      viper.GetBool("confirm"),
		Builder:      viper.GetString("builder"),
		BuilderImage: viper.GetString("builder-image"),
		Push:         viper.GetBool("push"),
		Platform:     viper.GetString("platform"),
	}
}

// Prompt the user with value of config members, allowing for interaractive changes.
// Skipped if not in an interactive terminal (non-TTY), or if --confirm false (agree to
// all prompts) was set (default).
func (c buildConfig) Prompt() (buildConfig, error) {
	imageName := deriveImage(c.Image, c.Registry, c.Path)
	if !interactiveTerminal() || !c.Confirm {
		return c, nil
	}

	bc := buildConfig{Verbose: c.Verbose}

	var qs = []*survey.Question{
		{
			Name: "path",
			Prompt: &survey.Input{
				Message: "Project path:",
				Default: c.Path,
			},
			Validate: survey.Required,
		},
		{
			Name: "image",
			Prompt: &survey.Input{
				Message: "Full image name (e.g. quay.io/boson/node-sample):",
				Default: imageName,
			},
			Validate: survey.Required,
		},
	}
	err := survey.Ask(qs, &bc)

	return bc, err
}
