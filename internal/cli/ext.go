package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"text/tabwriter"
	"time"

	"github.com/famarting/bigband/internal/extensions"
	"github.com/famarting/bigband/internal/ipc"
	"github.com/famarting/bigband/internal/paths"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// NewExtCmd registers `bigband ext` — operator surface for the supervisor:
// list status, start/stop/restart, tail logs, validate manifests.
func NewExtCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "ext",
		Short:   "Manage extensions supervised by the bigband daemon",
		GroupID: "run",
	}
	cmd.AddCommand(
		newExtListCmd(),
		newExtStartCmd(),
		newExtStopCmd(),
		newExtRestartCmd(),
		newExtLogsCmd(),
		newExtValidateCmd(),
		newExtEnableCmd(),
		newExtDisableCmd(),
	)
	return cmd
}

func newExtListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List extensions supervised by the daemon",
		RunE: func(cmd *cobra.Command, args []string) error {
			reply, err := ipc.Send(ipc.Cmd{Action: "ext_list"})
			if err != nil {
				return fmt.Errorf("cannot reach daemon: %w", err)
			}
			if !reply.OK {
				return fmt.Errorf("daemon error: %s", reply.Error)
			}
			var payload ipc.ExtListReply
			if err := json.Unmarshal(reply.Payload, &payload); err != nil {
				return fmt.Errorf("decoding reply: %w", err)
			}
			if len(payload.Extensions) == 0 {
				fmt.Println("(no extensions registered — drop a manifest at ~/.bigband-tasks/extensions/<name>/manifest.yaml)")
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tSTATUS\tPID\tRESTARTS\tLAST EXIT\tMANIFEST")
			now := time.Now().UTC()
			for _, e := range payload.Extensions {
				pid := "-"
				if e.PID > 0 {
					pid = fmt.Sprint(e.PID)
				}
				lastExit := "-"
				if !e.LastExitAt.IsZero() {
					lastExit = fmt.Sprintf("%s ago (code=%d)",
						now.Sub(e.LastExitAt).Round(time.Second), e.LastExitCode)
				} else if e.LastError != "" {
					lastExit = e.LastError
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\n",
					e.Name, e.Status, pid, e.Restarts, lastExit, shortenPath(e.ManifestPath))
			}
			return w.Flush()
		},
	}
}

func newExtStartCmd() *cobra.Command {
	return &cobra.Command{
		Use:               "start <name>",
		Short:             "Start a stopped extension",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: extNameCompletion,
		RunE: func(cmd *cobra.Command, args []string) error {
			return sendExtCmd("ext_start", args[0])
		},
	}
}

func newExtStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:               "stop <name>",
		Short:             "Stop a running extension (stays stopped until ext start)",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: extNameCompletion,
		RunE: func(cmd *cobra.Command, args []string) error {
			return sendExtCmd("ext_stop", args[0])
		},
	}
}

func newExtRestartCmd() *cobra.Command {
	return &cobra.Command{
		Use:               "restart <name>",
		Short:             "Restart an extension",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: extNameCompletion,
		RunE: func(cmd *cobra.Command, args []string) error {
			return sendExtCmd("ext_restart", args[0])
		},
	}
}

func newExtLogsCmd() *cobra.Command {
	var follow bool
	var tail int
	cmd := &cobra.Command{
		Use:               "logs <name>",
		Short:             "Show or follow an extension's stdout/stderr log",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: extNameCompletion,
		RunE: func(cmd *cobra.Command, args []string) error {
			info, err := lookupExtension(args[0])
			if err != nil {
				return err
			}
			if info.LogPath == "" {
				return fmt.Errorf("extension %s has no log path", args[0])
			}
			f, err := os.Open(info.LogPath)
			if err != nil {
				return fmt.Errorf("opening %s: %w", info.LogPath, err)
			}
			defer f.Close()
			if tail > 0 {
				return tailLastNExt(f, tail, follow)
			}
			if _, err := io.Copy(os.Stdout, f); err != nil {
				return err
			}
			if !follow {
				return nil
			}
			r := bufio.NewReader(f)
			for {
				line, err := r.ReadString('\n')
				if len(line) > 0 {
					fmt.Print(line)
				}
				if err == io.EOF {
					time.Sleep(200 * time.Millisecond)
					continue
				}
				if err != nil {
					return err
				}
			}
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "follow new lines as they arrive")
	cmd.Flags().IntVarP(&tail, "tail", "n", 0, "show only the last N lines")
	return cmd
}

func newExtValidateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "validate <path>",
		Short: "Validate a manifest file without contacting the daemon",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := extensions.LoadManifest(args[0])
			if err != nil {
				return err
			}
			fmt.Printf("ok: %s (name=%s command=%v)\n", args[0], m.Name, m.Command)
			return nil
		},
	}
}

func newExtEnableCmd() *cobra.Command {
	return &cobra.Command{
		Use:               "enable <name>",
		Short:             "Set enabled: true in the extension's manifest",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: extNameCompletion,
		RunE: func(cmd *cobra.Command, args []string) error {
			return setExtEnabled(args[0], true)
		},
	}
}

func newExtDisableCmd() *cobra.Command {
	return &cobra.Command{
		Use:               "disable <name>",
		Short:             "Set enabled: false in the extension's manifest",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: extNameCompletion,
		RunE: func(cmd *cobra.Command, args []string) error {
			return setExtEnabled(args[0], false)
		},
	}
}

// sendExtCmd is the small wrapper used by start/stop/restart.
func sendExtCmd(action, name string) error {
	reply, err := ipc.Send(ipc.Cmd{Action: action, Extension: name})
	if err != nil {
		return fmt.Errorf("cannot reach daemon: %w", err)
	}
	if !reply.OK {
		return fmt.Errorf("daemon error: %s", reply.Error)
	}
	fmt.Printf("ok: %s %s\n", action, name)
	return nil
}

// lookupExtension fetches one ExtensionInfo by name from the daemon.
func lookupExtension(name string) (*ipc.ExtensionInfo, error) {
	reply, err := ipc.Send(ipc.Cmd{Action: "ext_list"})
	if err != nil {
		return nil, fmt.Errorf("cannot reach daemon: %w", err)
	}
	if !reply.OK {
		return nil, fmt.Errorf("daemon error: %s", reply.Error)
	}
	var payload ipc.ExtListReply
	if err := json.Unmarshal(reply.Payload, &payload); err != nil {
		return nil, fmt.Errorf("decoding reply: %w", err)
	}
	for i := range payload.Extensions {
		if payload.Extensions[i].Name == name {
			return &payload.Extensions[i], nil
		}
	}
	return nil, fmt.Errorf("unknown extension: %s", name)
}

// extNameCompletion is a Cobra ValidArgsFunction that pulls extension names
// from the daemon for shell completion.
func extNameCompletion(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	reply, err := ipc.Send(ipc.Cmd{Action: "ext_list"})
	if err != nil || !reply.OK {
		return nil, cobra.ShellCompDirectiveError
	}
	var payload ipc.ExtListReply
	if err := json.Unmarshal(reply.Payload, &payload); err != nil {
		return nil, cobra.ShellCompDirectiveError
	}
	out := make([]string, 0, len(payload.Extensions))
	for _, e := range payload.Extensions {
		out = append(out, e.Name)
	}
	return out, cobra.ShellCompDirectiveNoFileComp
}

// setExtEnabled rewrites the manifest's enabled field. The supervisor picks up
// the change via fsnotify, so no IPC ping is needed.
func setExtEnabled(name string, enabled bool) error {
	manifestPath := filepath.Join(paths.Root(), "extensions", name, extensions.ManifestFilename)
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return fmt.Errorf("reading %s: %w", manifestPath, err)
	}
	// Decode into yaml.Node so we preserve comments and unrelated fields.
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("parsing %s: %w", manifestPath, err)
	}
	if err := setYAMLBoolField(&doc, "enabled", enabled); err != nil {
		return err
	}
	out, err := yaml.Marshal(&doc)
	if err != nil {
		return err
	}
	if err := os.WriteFile(manifestPath, out, 0600); err != nil {
		return err
	}
	fmt.Printf("set enabled=%v in %s\n", enabled, manifestPath)
	return nil
}

// setYAMLBoolField updates a top-level bool field in a YAML mapping document
// in place, preserving comments. Adds the field at the end if not present.
func setYAMLBoolField(doc *yaml.Node, key string, val bool) error {
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return fmt.Errorf("manifest is not a YAML document")
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return fmt.Errorf("manifest root must be a mapping")
	}
	valStr := "false"
	if val {
		valStr = "true"
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == key {
			root.Content[i+1].Value = valStr
			root.Content[i+1].Tag = "!!bool"
			return nil
		}
	}
	root.Content = append(root.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Value: valStr, Tag: "!!bool"},
	)
	return nil
}

// shortenPath replaces the user's home prefix with ~ for compact display.
func shortenPath(p string) string {
	if p == "" {
		return "-"
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p
	}
	if len(p) >= len(home) && p[:len(home)] == home {
		return "~" + p[len(home):]
	}
	return p
}

// tailLastNExt mirrors the helper in cmd/bigband-slack/service.go but lives
// here so internal/cli stays self-contained.
func tailLastNExt(f *os.File, n int, follow bool) error {
	data, err := io.ReadAll(f)
	if err != nil {
		return err
	}
	count := 0
	start := len(data)
	for i := len(data) - 1; i >= 0; i-- {
		if data[i] == '\n' {
			count++
			if count > n {
				start = i + 1
				break
			}
		}
	}
	if count <= n {
		start = 0
	}
	if _, err := os.Stdout.Write(data[start:]); err != nil {
		return err
	}
	if !follow {
		return nil
	}
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadString('\n')
		if len(line) > 0 {
			fmt.Print(line)
		}
		if err == io.EOF {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		if err != nil {
			return err
		}
	}
}
