package main

import (
	"flag"
	"fmt"
	"github.com/fatih/color"
	"io"
	"os"
	"osv-detector/internal"
	"osv-detector/internal/configer"
	"osv-detector/internal/reporter"
	"osv-detector/pkg/database"
	"osv-detector/pkg/lockfile"
	"path"
	"sort"
)

// these come from goreleaser
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func ecosystemDatabaseURL(ecosystem internal.Ecosystem) string {
	return fmt.Sprintf("https://osv-vulnerabilities.storage.googleapis.com/%s/all.zip", ecosystem)
}

type OSVDatabases []database.DB

func contains(items []string, value string) bool {
	for _, item := range items {
		if value == item {
			return true
		}
	}

	return false
}

func (dbs OSVDatabases) transposePkgResults(
	pkg internal.PackageDetails,
	ignores []string,
	packageIndex int,
	allVulns [][]database.Vulnerabilities,
) reporter.PackageDetailsWithVulnerabilities {
	vulnerabilities := make(database.Vulnerabilities, 0)
	ignored := make(database.Vulnerabilities, 0)

	for _, vulns1 := range allVulns {
		vulns := vulns1[packageIndex]

		for _, vulnerability := range vulns {
			// skip vulnerabilities that were already included from a previous database
			if vulnerabilities.Includes(vulnerability) || ignored.Includes(vulnerability) {
				continue
			}

			if contains(ignores, vulnerability.ID) {
				ignored = append(ignored, vulnerability)
			} else {
				vulnerabilities = append(vulnerabilities, vulnerability)
			}
		}
	}

	return reporter.PackageDetailsWithVulnerabilities{
		PackageDetails:  pkg,
		Vulnerabilities: vulnerabilities,
		Ignored:         ignored,
	}
}

func (dbs OSVDatabases) check(r *reporter.Reporter, lockf lockfile.Lockfile, ignores []string) reporter.Report {
	report := reporter.Report{
		Lockfile: lockf,
		Packages: make([]reporter.PackageDetailsWithVulnerabilities, 0, len(lockf.Packages)),
	}

	vulns := make([][]database.Vulnerabilities, 0, len(dbs))

	for _, db := range dbs {
		results, err := db.Check(lockf.Packages)

		if err != nil {
			r.PrintError(color.RedString(fmt.Sprintf(
				"  an api error occurred while trying to check the packages listed in %s: %v\n",
				lockf.FilePath,
				err,
			)))

			continue
		}

		vulns = append(vulns, results)
	}

	for i, pkg := range lockf.Packages {
		report.Packages = append(
			report.Packages,
			dbs.transposePkgResults(pkg, ignores, i, vulns),
		)
	}

	return report
}

func loadEcosystemDatabases(r *reporter.Reporter, ecosystems []internal.Ecosystem, offline bool) (OSVDatabases, error) {
	dbs := make(OSVDatabases, 0, len(ecosystems))

	r.PrintText("Loading OSV databases for the following ecosystems:\n")

	for _, ecosystem := range ecosystems {
		r.PrintText(fmt.Sprintf("  %s", ecosystem))
		archiveURL := ecosystemDatabaseURL(ecosystem)

		db, err := database.NewZippedDB(archiveURL, offline)

		if err != nil {
			return dbs, fmt.Errorf("could not load database: %w", err)
		}

		count := len(db.Vulnerabilities(true))

		r.PrintText(fmt.Sprintf(
			" (%s %s, including withdrawn - last updated %s)\n",
			color.YellowString("%d", count),
			reporter.Form(count, "vulnerability", "vulnerabilities"),
			db.UpdatedAt,
		))

		dbs = append(dbs, *db)
	}

	r.PrintText("\n")

	return dbs, nil
}

func cacheAllEcosystemDatabases(r *reporter.Reporter) error {
	ecosystems := lockfile.KnownEcosystems()

	_, err := loadEcosystemDatabases(r, ecosystems, false)

	return err
}

const parseAsCsvFile = "csv-file"
const parseAsCsvRow = "csv-row"

func findLockfiles(r *reporter.Reporter, pathToLockOrDirectory string, parseAs string) []string {
	lockfiles := make([]string, 0, 1)
	file, err := os.Open(pathToLockOrDirectory)

	if err == nil {
		info, err := file.Stat()

		if err == nil {
			if info.IsDir() {
				dirs, err := file.ReadDir(-1)

				if err == nil {
					for _, dir := range dirs {
						if dir.IsDir() {
							continue
						}

						if parseAs != parseAsCsvFile {
							if p, _ := lockfile.FindParser(dir.Name(), parseAs); p == nil {
								continue
							}
						}

						lockfiles = append(lockfiles, path.Join(pathToLockOrDirectory, dir.Name()))
					}
				}
			} else {
				lockfiles = append(lockfiles, pathToLockOrDirectory)
			}
		}
	}

	if err != nil {
		r.PrintError(fmt.Sprintf("Error reading %s: %v\n", pathToLockOrDirectory, err))
	}

	sort.Slice(lockfiles, func(i, j int) bool {
		return lockfiles[i] < lockfiles[j]
	})

	return lockfiles
}

func findAllLockfiles(r *reporter.Reporter, pathsToCheck []string, parseAs string) []string {
	var paths []string

	if parseAs == parseAsCsvRow {
		return []string{"-"}
	}

	for _, pathToLockOrDirectory := range pathsToCheck {
		for _, p := range findLockfiles(r, pathToLockOrDirectory, parseAs) {
			paths = append(paths, path.Clean(p))
		}
	}

	return paths
}

func parseLockfile(pathToLock string, parseAs string, args []string) (lockfile.Lockfile, error) {
	if parseAs == parseAsCsvRow {
		l, err := lockfile.FromCSVRows(pathToLock, parseAs, args)

		if err != nil {
			err = fmt.Errorf("%w", err)
		}

		return l, err
	}
	if parseAs == parseAsCsvFile {
		l, err := lockfile.FromCSVFile(pathToLock, parseAs)

		if err != nil {
			err = fmt.Errorf("%w", err)
		}

		return l, err
	}

	l, err := lockfile.Parse(pathToLock, parseAs)

	if err != nil {
		err = fmt.Errorf("%w", err)
	}

	return l, err
}

type stringsFlag []string

func (s *stringsFlag) String() string {
	return fmt.Sprint(*s)
}

func (s *stringsFlag) Set(value string) error {
	*s = append(*s, value)

	return nil
}

func allIgnores(global, local []string) []string {
	ignores := make(
		[]string,
		0,
		// len cannot return negative numbers, but the types can't reflect that
		uint64(len(global))+uint64(len(local)),
	)

	ignores = append(ignores, global...)
	ignores = append(ignores, local...)

	return ignores
}

type lockfileAndConfigOrErr struct {
	lockf  lockfile.Lockfile
	config configer.Config
	err    error
}

func readAllLockfiles(
	pathsToLocks []string,
	parseAs string,
	args []string,
	checkForLocalConfig bool,
	config configer.Config,
) []lockfileAndConfigOrErr {
	lockfiles := make([]lockfileAndConfigOrErr, 0, len(pathsToLocks))

	for _, pathToLock := range pathsToLocks {
		if checkForLocalConfig {
			base := path.Dir(pathToLock)
			con, err := configer.Find(base)

			if err != nil {
				lockfiles = append(lockfiles, lockfileAndConfigOrErr{lockfile.Lockfile{}, config, err})

				continue
			}

			config = con
		}

		lockf, err := parseLockfile(pathToLock, parseAs, args)
		lockfiles = append(lockfiles, lockfileAndConfigOrErr{lockf, config, err})
	}

	return lockfiles
}

func collectEcosystems(files []lockfileAndConfigOrErr) []internal.Ecosystem {
	var ecosystems []internal.Ecosystem

	for _, result := range files {
		if result.err != nil {
			continue
		}

		for _, ecosystem := range result.lockf.Packages.Ecosystems() {
			alreadyExists := false

			for _, eco := range ecosystems {
				if alreadyExists {
					continue
				}

				if eco == ecosystem {
					alreadyExists = true
				}
			}

			if alreadyExists {
				continue
			}

			ecosystems = append(ecosystems, ecosystem)
		}
	}

	return ecosystems
}

func loadDatabases(
	r *reporter.Reporter,
	ecosystems []internal.Ecosystem,
	listPackages bool,
	useDatabases bool,
	useAPI bool,
	batchSize int,
	offline bool,
) (OSVDatabases, bool) {
	var dbs OSVDatabases

	// an easy dirty little optimisation: we don't need any databases
	// if we're going to be listing packages, so return the empty slice
	if listPackages {
		return dbs, false
	}

	errored := false

	if useDatabases {
		loaded, err := loadEcosystemDatabases(r, ecosystems, offline)

		if err != nil {
			r.PrintDatabaseLoadErr(err)
			errored = true
		} else {
			dbs = append(dbs, loaded...)
		}
	}

	if useAPI {
		db, err := database.NewAPIDB("https://api.osv.dev/v1", batchSize, offline)

		if err != nil {
			r.PrintDatabaseLoadErr(err)
			errored = true
		} else {
			dbs = append(dbs, db)
		}
	}

	return dbs, errored
}

func run(args []string, stdout, stderr io.Writer) int {
	var ignores stringsFlag
	cli := flag.NewFlagSet(os.Args[0], flag.ExitOnError)

	offline := cli.Bool("offline", false, "Perform checks using only the cached databases on disk")
	parseAs := cli.String("parse-as", "", "Name of a supported lockfile to parse the input files as")
	configPath := cli.String("config", "", "Path to a config file to use for all lockfiles")
	noConfig := cli.Bool("no-config", false, "Disable loading of any config files")
	printVersion := cli.Bool("version", false, "Print version information")
	listEcosystems := cli.Bool("list-ecosystems", false, "List all of the known ecosystems that are supported by the detector")
	listPackages := cli.Bool("list-packages", false, "List the packages that are parsed from the input files")
	cacheAllDatabases := cli.Bool("cache-all-databases", false, "Cache all the known ecosystem databases for offline use")
	outputAsJSON := cli.Bool("json", false, "Output the results in JSON format")
	useDatabases := cli.Bool("use-dbs", true, "Use the databases from osv.dev to check for known vulnerabilities")
	useAPI := cli.Bool("use-api", false, "Use the osv.dev API to check for known vulnerabilities")
	batchSize := cli.Int("batch-size", 1000, "The number of packages to include in each batch when using the api database")

	cli.Var(&ignores, "ignore", `ID of an OSV to ignore when determining exit codes.
This flag can be passed multiple times to ignore different vulnerabilities`)

	// cli is set for ExitOnError so this will never return an error
	_ = cli.Parse(args)

	r := reporter.New(stdout, stderr, *outputAsJSON)
	if *outputAsJSON {
		defer r.PrintJSONResults()
	}

	if *printVersion {
		r.PrintText(fmt.Sprintf("osv-detector %s (%s, commit %s)\n", version, date, commit))

		return 0
	}

	if *cacheAllDatabases {
		err := cacheAllEcosystemDatabases(r)

		if err != nil {
			r.PrintDatabaseLoadErr(err)

			return 127
		}

		return 0
	}

	if *listEcosystems {
		r.PrintKnownEcosystems()

		return 0
	}

	if *parseAs != "" && *parseAs != parseAsCsvFile && *parseAs != parseAsCsvRow {
		if parser, parsedAs := lockfile.FindParser("", *parseAs); parser == nil {
			r.PrintError(fmt.Sprintf("Don't know how to parse files as \"%s\" - supported values are:\n", parsedAs))

			for _, s := range lockfile.ListParsers() {
				r.PrintError(fmt.Sprintf("  %s\n", s))
			}

			r.PrintError(fmt.Sprintf("  %s\n", parseAsCsvFile))
			r.PrintError(fmt.Sprintf("  %s\n", parseAsCsvRow))

			return 127
		}
	}

	pathsToLocks := findAllLockfiles(r, cli.Args(), *parseAs)

	if len(pathsToLocks) == 0 {
		r.PrintError(
			"You must provide at least one path to either a lockfile or a directory containing a lockfile (see --help for usage and flags)\n",
		)

		return 127
	}

	exitCode := 0

	var config configer.Config
	loadLocalConfig := !*noConfig

	// if we're listing packages, then we don't need to do _any_ config loading
	if *listPackages {
		loadLocalConfig = false
	} else if loadLocalConfig && *configPath != "" {
		con, err := configer.Load(*configPath)

		if err != nil {
			r.PrintError(fmt.Sprintf("Error, %s\n", err))

			return 127
		}

		config = con
		loadLocalConfig = false
	}

	files := readAllLockfiles(pathsToLocks, *parseAs, cli.Args(), loadLocalConfig, config)

	ecosystems := collectEcosystems(files)

	dbs, errored := loadDatabases(
		r,
		ecosystems,
		*listPackages,
		*useDatabases,
		*useAPI,
		*batchSize,
		*offline,
	)

	if errored {
		exitCode = 127
	}

	for i, result := range files {
		if i >= 1 {
			r.PrintText("\n")
		}

		if result.err != nil {
			r.PrintError(fmt.Sprintf("Error, %s\n", result.err))
			exitCode = 127

			continue
		}

		config := result.config
		lockf := result.lockf

		r.PrintText(fmt.Sprintf(
			"%s: found %s %s\n",
			color.MagentaString("%s", lockf.FilePath),
			color.YellowString("%d", len(lockf.Packages)),
			reporter.Form(len(lockf.Packages), "package", "packages"),
		))

		if *listPackages {
			r.PrintResult(lockf)

			continue
		}

		// an empty FilePath means we didn't load a config
		if config.FilePath != "" {
			r.PrintText(fmt.Sprintf(
				"  Using config at %s (%s)\n",
				color.MagentaString(config.FilePath),
				color.YellowString("%d %s",
					len(config.Ignore),
					reporter.Form(len(config.Ignore), "ignore", "ignores"),
				),
			))
		}

		report := dbs.check(r, lockf, allIgnores(config.Ignore, ignores))

		r.PrintResult(report)

		if report.HasKnownVulnerabilities() && exitCode == 0 {
			exitCode = 1
		}
	}

	return exitCode
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}
