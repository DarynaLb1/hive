package main

import (
    "context"
    "errors"
    "flag"
    "fmt"
    "log/slog"
    "os"
    "os/signal"
    "regexp"
    "sort"
    "strings"
    "time"

    "github.com/ethereum/hive/internal/libdocker"
    "github.com/ethereum/hive/internal/libhive"
    "github.com/lmittmann/tint"
)

// BuildArgs represents a map of build arguments for Docker
type BuildArgs map[string]string

// String implements the Stringer interface for BuildArgs
func (args BuildArgs) String() string {
    var kv []string
    for k, v := range args {
        kv = append(kv, fmt.Sprintf("%s=%s", k, v))
    }
    sort.Strings(kv)
    return strings.Join(kv, ",")
}

// Set implements flag.Value interface for BuildArgs
func (args BuildArgs) Set(value string) error {
    parts := strings.SplitN(value, "=", 2)
    if len(parts) != 2 {
        return fmt.Errorf("invalid build argument format: %s, expected ARG=VALUE", value)
    }
    args[parts[0]] = parts[1]
    return nil
}

// Config holds all configuration options for the Hive simulator
type Config struct {
    TestResultsRoot       string
    LogLevel              int
    DockerEndpoint        string
    DockerNoCache         string
    DockerPull            bool
    DockerOutput          bool
    DockerBuildOutput     bool
    SimPattern            string
    SimTestPattern        string
    SimParallelism        int
    SimRandomSeed         int
    SimTestLimit          int // Deprecated
    SimTimeLimit          time.Duration
    SimLogLevel           int
    SimDevMode            bool
    SimDevModeAPIEndpoint string
    UseCredHelper         bool
    ClientsFile           string
    Clients               string
    ClientTimeout         time.Duration
    SimBuildArgs          BuildArgs
}

func main() {
    config := parseFlags()
    configureLogger(config.LogLevel)
    
    // Set GODEBUG for multipart handling
    if err := os.Setenv("GODEBUG", "multipartmaxparts=20000"); err != nil {
        fatal("failed to set GODEBUG:", err)
    }

    ctx, cancel := setupSignalHandling()
    defer cancel()

    inv, err := libhive.LoadInventory(".")
    if err != nil {
        fatal("failed to load inventory:", err)
    }

    simulators, err := prepareSimulators(inv, config)
    if err != nil {
        fatal("failed to prepare simulators:", err)
    }

    builder, cb, err := setupDocker(config, inv)
    if err != nil {
        fatal("failed to setup docker:", err)
    }

    runner := libhive.NewRunner(inv, builder, cb)
    clientList, err := prepareClients(inv, config)
    if err != nil {
        fatal("failed to prepare clients:", err)
    }

    hiveInfo := libhive.HiveInfo{
        Command:    os.Args,
        ClientFile: clientList,
    }

    if err := runner.Build(ctx, clientList, simulators, config.SimBuildArgs); err != nil {
        fatal("build failed:", err)
    }

    if config.SimDevMode {
        runner.RunDevMode(ctx, createSimEnv(config), config.SimDevModeAPIEndpoint, hiveInfo)
        return
    }

    runSimulations(ctx, runner, simulators, config, hiveInfo)
}

// parseFlags parses command line flags and returns a Config
func parseFlags() Config {
    config := Config{
        SimBuildArgs: make(BuildArgs),
    }

    flag.StringVar(&config.TestResultsRoot, "results-root", "workspace/logs", "Target directory for results files and logs")
    flag.IntVar(&config.LogLevel, "loglevel", 3, "Log level for system events (0-5)")
    flag.StringVar(&config.DockerEndpoint, "docker.endpoint", "", "Endpoint of the local Docker daemon")
    flag.StringVar(&config.DockerNoCache, "docker.nocache", "", "Regular expression selecting docker images to forcibly rebuild")
    flag.BoolVar(&config.DockerPull, "docker.pull", false, "Refresh base images when building images")
    flag.BoolVar(&config.DockerOutput, "docker.output", false, "Relay all docker output to stderr")
    flag.BoolVar(&config.DockerBuildOutput, "docker.buildoutput", false, "Relay only docker build output to stderr")
    flag.StringVar(&config.SimPattern, "sim", "", "Regular expression selecting simulators to run")
    flag.StringVar(&config.SimTestPattern, "sim.limit", "", "Regular expression selecting tests/suites")
    flag.IntVar(&config.SimParallelism, "sim.parallelism", 1, "Max number of parallel clients/containers")
    flag.IntVar(&config.SimRandomSeed, "sim.randomseed", 0, "Randomness seed number")
    flag.IntVar(&config.SimTestLimit, "sim.testlimit", 0, "[DEPRECATED] Max number of tests per client")
    flag.DurationVar(&config.SimTimeLimit, "sim.timelimit", 0, "Simulation timeout")
    flag.IntVar(&config.SimLogLevel, "sim.loglevel", 3, "Log level for client instances (0-5)")
    flag.BoolVar(&config.SimDevMode, "dev", false, "Run in development mode with API endpoint")
    flag.StringVar(&config.SimDevModeAPIEndpoint, "dev.addr", "127.0.0.1:3000", "Simulator API endpoint")
    flag.BoolVar(&config.UseCredHelper, "docker.cred-helper", false, "Use locally-configured credential helper for docker auth")
    flag.StringVar(&config.ClientsFile, "client-file", "", "YAML file containing client configurations")
    flag.StringVar(&config.Clients, "client", "go-ethereum", "Comma-separated list of clients")
    flag.DurationVar(&config.ClientTimeout, "client.checktimelimit", 3*time.Minute, "Timeout for client RPC port opening")
    flag.Var(&config.SimBuildArgs, "sim.buildarg", "Build argument for simulator image (ARGNAME=VALUE)")

    flag.Parse()
    return config
}

// configureLogger sets up the logging system
func configureLogger(level int) {
    terminal := os.Getenv("TERM")
    handler := tint.NewHandler(os.Stderr, &tint.Options{
        Level:   convertLogLevel(level),
        NoColor: terminal == "" || terminal == "dumb",
    })
    slog.SetDefault(slog.New(handler))
}

// setupSignalHandling creates a context that cancels on interrupt
func setupSignalHandling() (context.Context, context.CancelFunc) {
    ctx, cancel := context.WithCancel(context.Background())
    sig := make(chan os.Signal, 1)
    signal.Notify(sig, os.Interrupt)
    go func() {
        <-sig
        cancel()
    }()
    return ctx, cancel
}

// prepareSimulators loads and matches simulators based on pattern
func prepareSimulators(inv *libhive.Inventory, config Config) ([]string, error) {
    if config.SimTestLimit > 0 {
        slog.Warn("Option --sim.testlimit is deprecated and will have no effect")
    }
    
    simList, err := inv.MatchSimulators(config.SimPattern)
    if err != nil {
        return nil, fmt.Errorf("bad simulator pattern: %w", err)
    }
    if config.SimPattern != "" && len(simList) == 0 {
        return nil, fmt.Errorf("no simulators found for pattern: %s", config.SimPattern)
    }
    if config.SimPattern != "" && config.SimDevMode {
        slog.Warn("--sim is ignored when using --dev mode")
        return nil, nil
    }
    return simList, nil
}

// setupDocker configures and connects to the Docker daemon
func setupDocker(config Config, inv *libhive.Inventory) (*libdocker.Builder, *libdocker.ContainerBackend, error) {
    dockerConfig := &libdocker.Config{
        Inventory:           inv,
        PullEnabled:         config.DockerPull,
        UseCredentialHelper: config.UseCredHelper,
    }
    
    if config.DockerNoCache != "" {
        re, err := regexp.Compile(config.DockerNoCache)
        if err != nil {
            return nil, nil, fmt.Errorf("bad docker nocache pattern: %w", err)
        }
        dockerConfig.NoCachePattern = re
    }
    
    if config.DockerOutput {
        dockerConfig.ContainerOutput = os.Stderr
        dockerConfig.BuildOutput = os.Stderr
    } else if config.DockerBuildOutput {
        dockerConfig.BuildOutput = os.Stderr
    }
    
    return libdocker.Connect(config.DockerEndpoint, dockerConfig)
}

// prepareClients loads and parses client configurations
func prepareClients(inv *libhive.Inventory, config Config) ([]libhive.ClientDesignator, error) {
    if config.ClientsFile == "" {
        return libhive.ParseClientList(inv, config.Clients)
    }
    
    clientList, err := parseClientsFile(inv, config.ClientsFile)
    if err != nil {
        return nil, err
    }
    
    if flagIsSet("client") {
        filter := strings.Split(config.Clients, ",")
        clientList = libhive.FilterClients(clientList, filter)
    }
    return clientList, nil
}

// createSimEnv creates a simulation environment from config
func createSimEnv(config Config) libhive.SimEnv {
    return libhive.SimEnv{
        LogDir:             config.TestResultsRoot,
        SimLogLevel:        config.SimLogLevel,
        SimTestPattern:     config.SimTestPattern,
        SimParallelism:     config.SimParallelism,
        SimRandomSeed:      config.SimRandomSeed,
        SimDurationLimit:   config.SimTimeLimit,
        ClientStartTimeout: config.ClientTimeout,
    }
}

// runSimulations executes all simulations and handles results
func runSimulations(ctx context.Context, runner *libhive.Runner, simulators []string, config Config, hiveInfo libhive.HiveInfo) {
    var failCount int
    for _, sim := range simulators {
        result, err := runner.Run(ctx, sim, createSimEnv(config), hiveInfo)
        if err != nil {
            fatal("simulation failed:", err)
        }
        failCount += result.TestsFailed
        slog.Info(fmt.Sprintf("Simulation %s completed", sim),
            "suites", result.Suites,
            "tests", result.Tests,
            "failed", result.TestsFailed)
    }

    switch failCount {
    case 0:
        slog.Info("All tests passed successfully")
    case 1:
        fatal("tests failed:", errors.New("1 test failed"))
    default:
        fatal("tests failed:", fmt.Errorf("%d tests failed", failCount))
    }
}

// fatal logs an error and exits with status 1
func fatal(args ...interface{}) {
    fmt.Fprintln(os.Stderr, args...)
    os.Exit(1)
}

// parseClientsFile reads and parses a YAML client configuration file
func parseClientsFile(inv *libhive.Inventory, file string) ([]libhive.ClientDesignator, error) {
    f, err := os.Open(file)
    if err != nil {
        return nil, fmt.Errorf("failed to open clients file: %w", err)
    }
    defer f.Close()
    return libhive.ParseClientListYAML(inv, f)
}

// flagIsSet checks if a flag was explicitly set
func flagIsSet(name string) bool {
    var found bool
    flag.Visit(func(f *flag.Flag) {
        if f.Name == name {
            found = true
        }
    })
    return found
}

// convertLogLevel maps 0-5 range to slog levels
func convertLogLevel(level int) slog.Level {
    switch level {
    case 0:
        return slog.Level(99) // Silent
    case 1:
        return slog.LevelError
    case 2:
        return slog.LevelWarn
    case 3:
        return slog.LevelInfo
    default:
        return slog.LevelDebug
    }
}
