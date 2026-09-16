package main

import (
	"os"

	"github.com/konflux-ci/gobsi/pkg/gobsi"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

func main() {

	rootCmd := &cobra.Command{
		Use:   "gobsi",
		Short: "Build OCI source container images",
		RunE:  run,
	}

	rootCmd.Flags().StringP("srpm-dir", "s", "", "directory of *.src.rpm files to add (searched recursively)")
	rootCmd.Flags().StringArrayP("extra-src-dir", "e", nil, "extra source directory to add as a layer")
	rootCmd.Flags().StringP("output", "o", "", "output OCI image layout directory (required)")
	rootCmd.Flags().BoolP("debug", "d", false, "enable debug logging")
	rootCmd.MarkFlagRequired("output")

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func run(cmd *cobra.Command, args []string) error {
	// Silence usage here so flag parsing errors still print help,
	// but runtime errors don't.
	cmd.SilenceUsage = true
	debug, _ := cmd.Flags().GetBool("debug")
	if debug {
		log.SetLevel(log.DebugLevel)
	}

	srpmDir, _ := cmd.Flags().GetString("srpm-dir")
	extraDirs, _ := cmd.Flags().GetStringArray("extra-src-dir")
	outputDir, _ := cmd.Flags().GetString("output")

	return gobsi.BuildSourceImage(gobsi.BuildConfig{
		SRPMDir:   srpmDir,
		ExtraDirs: extraDirs,
		OutputDir: outputDir,
	})
}
