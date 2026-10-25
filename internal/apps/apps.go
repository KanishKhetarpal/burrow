// Package apps opens programs on the owner's desktop for the "Open on PC"
// buttons — Chrome in a given profile, say.
//
// The agent runs as a service in session 0, and a process it starts there
// has no desktop: Chrome would run where nobody can see it. So each app is
// registered as an on-demand scheduled task that runs as the owner with an
// interactive token, and opening it runs that task, which Windows starts in
// the logged-in user's session.
package apps

import (
	"errors"
	"regexp"
	"strings"

	"burrow/internal/config"
)

var (
	// ErrUnknown means the name is not in burrow.yaml's apps.
	ErrUnknown = errors.New("apps: no such app")
	// ErrNoDesktop means nobody is logged into Windows, so there is no
	// desktop to open the program on.
	ErrNoDesktop = errors.New("apps: nobody is logged into Windows on the PC")
)

// taskFolder holds every task Burrow registers, so they are easy to find in
// Task Scheduler and to remove on uninstall.
const taskFolder = `\Burrow\`

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9 ._-]+`)

// TaskName is the scheduled task that opens an app.
func TaskName(appName string) string {
	clean := strings.TrimSpace(unsafeName.ReplaceAllString(appName, " "))
	clean = strings.Join(strings.Fields(clean), " ")
	if clean == "" {
		clean = "app"
	}
	return taskFolder + "Open " + clean
}

// Opener registers and opens apps.
type Opener interface {
	// Sync registers a task for every app in the config and removes tasks
	// for apps that are no longer listed.
	Sync(apps []config.App) error
	// Open starts the named app on the owner's desktop.
	Open(name string) error
}

var xmlEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")

// taskXML is the Task Scheduler definition for one app: no triggers (run on
// demand only), the owner's interactive token at normal privilege, and no
// time limit so the program is not killed after three days.
func taskXML(user, command, arguments string) string {
	return `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo>
    <Description>Opened by Burrow when a paired device taps its button.</Description>
  </RegistrationInfo>
  <Principals>
    <Principal id="Author">
      <UserId>` + xmlEscaper.Replace(user) + `</UserId>
      <LogonType>InteractiveToken</LogonType>
      <RunLevel>LeastPrivilege</RunLevel>
    </Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>Parallel</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <AllowStartOnDemand>true</AllowStartOnDemand>
    <Enabled>true</Enabled>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
  </Settings>
  <Actions Context="Author">
    <Exec>
      <Command>` + xmlEscaper.Replace(command) + `</Command>
      <Arguments>` + xmlEscaper.Replace(arguments) + `</Arguments>
    </Exec>
  </Actions>
</Task>
`
}
