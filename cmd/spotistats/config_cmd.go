package main

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/neovasili/spotistats/internal/config"
)

func runConfig(_ context.Context, args []string) error {
	fs := newFlagSet("config", "config")
	if err := fs.Parse(args); err != nil {
		return err
	}

	c := config.Load().Redacted()
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintf(tw, "region\t%s\t(%s)\n",
		orDefault(c.Region, "resolved from the AWS profile"), config.EnvRegion)
	fmt.Fprintf(tw, "table\t%s\t(%s)\n", orUnset(c.TableName), config.EnvTableName)
	fmt.Fprintf(tw, "dynamodb endpoint\t%s\t(%s)\n", orDefault(c.DDBEndpoint, "AWS"), config.EnvDDBEndpoint)
	fmt.Fprintf(tw, "timezone\t%s\t(%s)\n", c.Timezone, config.EnvTimezone)
	fmt.Fprintf(tw, "capture limit\t%d\t(%s)\n", c.CaptureLimit, config.EnvCaptureLimit)
	fmt.Fprintf(tw, "redirect URI\t%s\t(%s)\n", c.RedirectURI, config.EnvRedirectURI)
	fmt.Fprintf(tw, "ssm prefix\t%s\t(%s)\n", c.SSMPrefix, config.EnvSSMPrefix)
	if c.UsesLocalTokenFile() {
		fmt.Fprintf(tw, "token store\tfile: %s\t(%s)\n", c.TokenFile, config.EnvTokenFile)
	} else {
		fmt.Fprintf(tw, "token store\tSSM: %s\t(%s)\n", c.RefreshTokenParam(), config.EnvTokenFile)
	}
	fmt.Fprintf(tw, "client id\t%s\t(%s)\n", orDefault(c.ClientID, "from SSM"), config.EnvClientID)
	fmt.Fprintf(tw, "client secret\t%s\t(%s)\n", orDefault(c.ClientSecret, "from SSM"), config.EnvClientSecret)
	if err := tw.Flush(); err != nil {
		return err
	}

	if err := c.Validate(); err != nil {
		fmt.Printf("\nNot ready for storage commands:\n  %v\n", err)
	}
	return nil
}

func orUnset(s string) string { return orDefault(s, "(unset)") }

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
