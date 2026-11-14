// Command babysit commits and pushes staged changes, then shows the GitLab pipelines they trigger.
package main

import "github.com/joey-bednar/sandbox/internal/cli"

func main() {
	cli.Execute()
}
