package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/kyfd/changeguard/internal/evidence"
	"github.com/kyfd/changeguard/internal/store"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "changeguard-evidence:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: changeguard-evidence export|verify [options]")
	}
	switch args[0] {
	case "export":
		flags := flag.NewFlagSet("export", flag.ContinueOnError)
		dataPath := flags.String("data", "./data/dbguard.json", "ChangeGuard state file")
		changeID := flags.String("change", "", "change ID")
		output := flags.String("out", "", "bundle output file")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if *changeID == "" || *output == "" {
			return fmt.Errorf("export requires -change and -out")
		}
		data, err := store.OpenReadOnlySnapshot(*dataPath)
		if err != nil {
			return err
		}
		defer data.Close()
		change, err := data.Change(*changeID)
		if err != nil {
			return err
		}
		content, err := evidence.Export(evidence.Input{
			Change: change, Policies: data.PoliciesByOrganization(change.OrganizationID),
			Passports:         data.PassportsByChange(change.OrganizationID, change.ID),
			IntegrationEvents: data.IntegrationEventsByChange(change.OrganizationID, change.ID),
			OutcomeSignals:    data.OutcomeSignalsByChange(change.OrganizationID, change.ID),
			Audits:            data.AuditsByOrganizationAppendOrder(change.OrganizationID),
		})
		if err != nil {
			return err
		}
		return os.WriteFile(*output, content, 0o600)
	case "verify":
		flags := flag.NewFlagSet("verify", flag.ContinueOnError)
		input := flags.String("in", "", "bundle input file")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if *input == "" {
			return fmt.Errorf("verify requires -in")
		}
		content, err := os.ReadFile(*input)
		if err != nil {
			return err
		}
		if err := evidence.Verify(content); err != nil {
			return err
		}
		fmt.Println("evidence bundle verified")
		return nil
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}
