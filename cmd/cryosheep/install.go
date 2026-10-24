package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/PrPlanIT/CryoSheep/deploy/systemd"
	"github.com/PrPlanIT/CryoSheep/src/selfinstall"
	"github.com/PrPlanIT/CryoSheep/src/version"
)

func requireRoot(verb string) bool {
	if os.Geteuid() != 0 {
		fmt.Fprintf(os.Stderr, "cryosheep %s: must run as root\n", verb)
		return false
	}
	return true
}

func printReport(rep selfinstall.Report) {
	if len(rep) == 0 {
		fmt.Println("already current; nothing changed")
		return
	}
	for _, line := range rep {
		fmt.Println(line)
	}
}

// sameBuild reports whether the running binary is the build a release tag names.
func sameBuild(tag string) bool {
	if strings.HasPrefix(tag, "dev-") {
		sha := strings.TrimPrefix(tag, "dev-")
		return sha != "" && strings.HasPrefix(version.Commit, sha)
	}
	return tag == "v"+version.Version
}

func runInstall(args []string) int {
	fs := flag.NewFlagSet("install", flag.ExitOnError)
	dev := fs.Bool("dev", false, "install the newest main build instead of the newest release")
	source := fs.String("source", selfinstall.DefaultSource, "forge releases API for this project")
	_ = fs.Parse(args)
	if !requireRoot("install") {
		return 1
	}
	ch := selfinstall.Stable
	if *dev {
		ch = selfinstall.Dev
	}
	return fetchAndInstall(*source, ch, true, false)
}

func runUpdate(args []string) int {
	fs := flag.NewFlagSet("update", flag.ExitOnError)
	source := fs.String("source", selfinstall.DefaultSource, "forge releases API for this project")
	_ = fs.Parse(args)
	if !requireRoot("update") {
		return 1
	}
	if _, err := os.Stat(selfinstall.BinaryPath); err != nil {
		fmt.Fprintf(os.Stderr, "cryosheep update: not installed (%s missing); run cryosheep install\n", selfinstall.BinaryPath)
		return 1
	}
	return fetchAndInstall(*source, selfinstall.InstalledChannel(), false, true)
}

func fetchAndInstall(source string, ch selfinstall.Channel, enable, skipIfCurrent bool) int {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	client := selfinstall.NewClient()
	rel, err := selfinstall.Latest(ctx, client, source, ch)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cryosheep: %v\n", err)
		return 1
	}
	if skipIfCurrent && sameBuild(rel.Tag) {
		fmt.Printf("already on %s (%s channel)\n", rel.Tag, ch)
		return 0
	}
	bin, err := selfinstall.Fetch(ctx, client, rel, runtime.GOARCH)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cryosheep: %v\n", err)
		return 1
	}
	fmt.Printf("%s (%s channel), checksum verified\n", rel.Tag, ch)
	rep, err := selfinstall.Install(bin, systemd.Units, ch, enable)
	printReport(rep)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cryosheep: %v\n", err)
		return 1
	}
	return 0
}

func runRemove(args []string) int {
	fs := flag.NewFlagSet("remove", flag.ExitOnError)
	_ = fs.Parse(args)
	if !requireRoot("remove") {
		return 1
	}
	rep, err := selfinstall.Remove()
	printReport(rep)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cryosheep: %v\n", err)
		return 1
	}
	return 0
}
