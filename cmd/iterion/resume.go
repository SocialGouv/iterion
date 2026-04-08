package main

import (
	"github.com/SocialGouv/iterion/cli"
	"github.com/spf13/cobra"
)

var resumeOpts struct {
	runID       string
	file        string
	storeDir    string
	answersFile string
	answerFlags []string
	logLevel    string
}

var resumeCmd = &cobra.Command{
	Use:   "resume",
	Short: "Resume a paused run",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		opts := cli.ResumeOptions{
			RunID:       resumeOpts.runID,
			StoreDir:    resumeOpts.storeDir,
			AnswersFile: resumeOpts.answersFile,
			LogLevel:    resumeOpts.logLevel,
		}
		if len(resumeOpts.answerFlags) > 0 {
			answers, err := cli.ParseAnswerFlags(resumeOpts.answerFlags)
			if err != nil {
				return err
			}
			opts.Answers = answers
		}
		return cli.RunResumeWithFile(cmd.Context(), resumeOpts.file, opts, newPrinter())
	},
}

func init() {
	f := resumeCmd.Flags()
	f.StringVar(&resumeOpts.runID, "run-id", "", "Run to resume")
	f.StringVar(&resumeOpts.file, "file", "", "Workflow file (.iter)")
	f.StringVar(&resumeOpts.storeDir, "store-dir", "", "Store directory (default: .iterion)")
	f.StringVar(&resumeOpts.answersFile, "answers-file", "", "JSON file with answers")
	f.StringArrayVar(&resumeOpts.answerFlags, "answer", nil, "Set answer (key=value, repeatable)")
	f.StringVar(&resumeOpts.logLevel, "log-level", "", "Log verbosity: error, warn, info, debug, trace")
	mustMarkRequired(resumeCmd, "run-id", "file")
	rootCmd.AddCommand(resumeCmd)
}
