package cli

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/famarting/bigband/internal/config"
	"github.com/famarting/bigband/internal/paths"
)

// askName prompts for a task/template name, slugifies it, and asks the user to
// confirm a corrected slug if it differs from the input. Returns the validated
// name (matching config.IsValidName) or an error.
func askName(r *bufio.Reader, label string) (string, error) {
	ask := makeAsk(r)
	for {
		raw, err := ask(label)
		if err != nil {
			return "", err
		}
		if raw == "" {
			return "", fmt.Errorf("name is required")
		}
		slug := config.Slugify(raw)
		if slug == "" {
			fmt.Println("  → name has no valid characters; try letters/digits/dashes")
			continue
		}
		if slug != raw {
			confirm, err := ask(fmt.Sprintf("  → using %q (lowercase, dashes only). Press enter to accept, or type a different name", slug))
			if err != nil {
				return "", err
			}
			if confirm != "" {
				slug = config.Slugify(confirm)
			}
		}
		if !config.IsValidName(slug) {
			fmt.Printf("  → %q is not a valid name; must match [a-z0-9][a-z0-9-_]*\n", slug)
			continue
		}
		return slug, nil
	}
}

// normalizeName slugifies raw and validates the result. If the slug differs
// from raw it prints a notice so non-interactive callers see the rewrite.
// Returns an error if no valid name can be derived.
func normalizeName(raw string) (string, error) {
	slug := config.Slugify(raw)
	if slug == "" {
		return "", fmt.Errorf("name %q has no valid characters; must contain [a-z0-9-_]", raw)
	}
	if !config.IsValidName(slug) {
		return "", fmt.Errorf("derived name %q is not valid; must match [a-z0-9][a-z0-9-_]*", slug)
	}
	if slug != raw {
		fmt.Fprintf(os.Stderr, "note: using normalized name %q (was %q)\n", slug, raw)
	}
	return slug, nil
}

// validateUniqueName returns an error if the name already exists as a task or
// template in the loaded config (used to fail fast before writing YAML).
func validateUniqueName(name string) error {
	cfg, err := config.Load(paths.Config())
	if err != nil {
		// If the config doesn't load yet (e.g. first run), skip the check.
		return nil
	}
	if cfg.TaskByName(name) != nil {
		return fmt.Errorf("a task named %q already exists", name)
	}
	if cfg.TemplateByName(name) != nil {
		return fmt.Errorf("a template named %q already exists", name)
	}
	return nil
}

func newReader() *bufio.Reader {
	return bufio.NewReader(os.Stdin)
}

func makeAsk(r *bufio.Reader) func(string) (string, error) {
	return func(prompt string) (string, error) {
		fmt.Print(prompt + ": ")
		line, err := r.ReadString('\n')
		return strings.TrimSpace(line), err
	}
}

func makeAskMulti(r *bufio.Reader) func(string) ([]string, error) {
	return func(prompt string) ([]string, error) {
		fmt.Println(prompt + " (one per line, blank line to finish):")
		var lines []string
		for {
			fmt.Print("  > ")
			line, err := r.ReadString('\n')
			if err != nil {
				return lines, err
			}
			line = strings.TrimSpace(line)
			if line == "" {
				break
			}
			lines = append(lines, line)
		}
		return lines, nil
	}
}
