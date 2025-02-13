package main

import (
	"fmt"
	"os"

	"github.com/bitrise-io/go-steputils/tools"
	"github.com/bitrise-io/go-steputils/v2/stepconf"
	v1log "github.com/bitrise-io/go-utils/log"
	"github.com/bitrise-io/go-utils/retry"
	"github.com/bitrise-io/go-utils/v2/command"
	"github.com/bitrise-io/go-utils/v2/env"
	"github.com/bitrise-io/go-utils/v2/log"
	"github.com/bitrise-io/go-xcode/devportalservice"
	"github.com/bitrise-io/go-xcode/utility"
	"github.com/bitrise-io/go-xcode/v2/autocodesign"
	"github.com/bitrise-io/go-xcode/v2/autocodesign/certdownloader"
	"github.com/bitrise-io/go-xcode/v2/autocodesign/codesignasset"
	"github.com/bitrise-io/go-xcode/v2/autocodesign/devportalclient"
	"github.com/bitrise-io/go-xcode/v2/autocodesign/keychain"
	"github.com/bitrise-io/go-xcode/v2/autocodesign/localcodesignasset"
	"github.com/bitrise-io/go-xcode/v2/autocodesign/projectmanager"
	"github.com/bitrise-io/go-xcode/v2/codesign"
	"github.com/bitrise-io/go-xcode/xcodebuild"
)

func failf(format string, args ...interface{}) {
	v1log.Errorf(format, args...)
	os.Exit(1)
}

func main() {
	// Parse and validate inputs
	var cfg Config
	parser := stepconf.NewInputParser(env.NewRepository())
	if err := parser.Parse(&cfg); err != nil {
		failf("Config: %s", err)
	}
	stepconf.Print(cfg)

	logger := log.NewLogger()
	logger.EnableDebugLog(cfg.VerboseLog)
	v1log.SetEnableDebugLog(cfg.VerboseLog) // for compatibility

	cmdFactory := command.NewFactory(env.NewRepository())

	xcodebuildVersion, err := utility.GetXcodeVersion()
	if err != nil {
		failf("Failed to determine Xcode version: %s", err)
	}
	logger.Printf("%s (%s)", xcodebuildVersion.Version, xcodebuildVersion.BuildVersion)

	logger.Println()
	if xcodebuildVersion.MajorVersion >= 11 {
		// Resolve Swift package dependencies, so running -showBuildSettings is faster
		// Specifying a scheme is required for workspaces
		resolveDepsCmd := xcodebuild.NewResolvePackagesCommandModel(cfg.ProjectPath, cfg.Scheme, cfg.Configuration)
		if err := resolveDepsCmd.Run(); err != nil {
			logger.Warnf("%s", err)
		}
	}

	// Analyze project
	fmt.Println()
	logger.Infof("Analyzing project")
	project, err := projectmanager.NewProject(projectmanager.InitParams{
		ProjectOrWorkspacePath: cfg.ProjectPath,
		SchemeName:             cfg.Scheme,
		ConfigurationName:      cfg.Configuration,
	})
	if err != nil {
		failf(err.Error())
	}

	appLayout, err := project.GetAppLayout(cfg.SignUITestTargets)
	if err != nil {
		failf(err.Error())
	}

	// Create Apple developer Portal client
	authType, err := parseAuthType(cfg.BitriseConnection)
	if err != nil {
		failf("Invalid input: unexpected value for Bitrise Apple Developer Connection (%s)", cfg.BitriseConnection)
	}

	var connection *devportalservice.AppleDeveloperConnection
	isRunningOnBitrise := cfg.BuildURL != "" && cfg.BuildAPIToken != ""

	switch {
	case !isRunningOnBitrise:
		fmt.Println()
		failf(`Connected Apple Developer Portal Account not found. Step is not running on bitrise.io: BITRISE_BUILD_URL and BITRISE_BUILD_API_TOKEN envs are not set.
               For testing purposes please provide BITRISE_BUILD_URL as json file (file://path-to-json) while setting BITRISE_BUILD_API_TOKEN to any non-empty string`)
	default:
		f := devportalclient.NewFactory(logger)
		c, err := f.CreateBitriseConnection(cfg.BuildURL, cfg.BuildAPIToken)
		if err != nil {
			failf(err.Error())
		}
		connection = c
	}

	codesignInputs := codesign.Input{
		AuthType:                  authType,
		DistributionMethod:        cfg.Distribution,
		CertificateURLList:        cfg.CertificateURLList,
		CertificatePassphraseList: cfg.CertificatePassphraseList,
		KeychainPath:              cfg.KeychainPath,
		KeychainPassword:          cfg.KeychainPassword,
	}

	codesignConfig, err := codesign.ParseConfig(codesignInputs, cmdFactory)
	if err != nil {
		failf(err.Error())
	}
	appleAuthCredentials, err := codesign.SelectConnectionCredentials(authType, connection, logger)
	if err != nil {
		failf(err.Error())
	}

	keychain, err := keychain.New(cfg.KeychainPath, cfg.KeychainPassword, cmdFactory)
	if err != nil {
		failf(fmt.Sprintf("failed to initialize keychain: %s", err))
	}

	devPortalClientFactory := devportalclient.NewFactory(logger)
	certDownloader := certdownloader.NewDownloader(codesignConfig.CertificatesAndPassphrases, retry.NewHTTPClient().StandardClient())
	assetWriter := codesignasset.NewWriter(*keychain)
	localCodesignAssetManager := localcodesignasset.NewManager(localcodesignasset.NewProvisioningProfileProvider(), localcodesignasset.NewProvisioningProfileConverter())

	devPortalClient, err := devPortalClientFactory.Create(appleAuthCredentials, cfg.TeamID)
	if err != nil {
		failf(err.Error())
	}

	// Create codesign manager
	manager := autocodesign.NewCodesignAssetManager(devPortalClient, certDownloader, assetWriter, localCodesignAssetManager)

	// Auto codesign
	distribution := cfg.DistributionType()
	var testDevices []devportalservice.TestDevice
	if cfg.RegisterTestDevices {
		testDevices = connection.TestDevices
	}
	codesignAssetsByDistributionType, err := manager.EnsureCodesignAssets(appLayout, autocodesign.CodesignAssetsOpts{
		DistributionType:       distribution,
		BitriseTestDevices:     testDevices,
		MinProfileValidityDays: cfg.MinProfileDaysValid,
		VerboseLog:             cfg.VerboseLog,
	})
	if err != nil {
		failf(fmt.Sprintf("Automatic code signing failed: %s", err))
	}

	if err := project.ForceCodesignAssets(distribution, codesignAssetsByDistributionType); err != nil {
		failf(fmt.Sprintf("Failed to force codesign settings: %s", err))
	}

	// Export output
	fmt.Println()
	logger.Infof("Exporting outputs")

	teamID := codesignAssetsByDistributionType[distribution].Certificate.TeamID
	outputs := map[string]string{
		"BITRISE_EXPORT_METHOD":  cfg.Distribution,
		"BITRISE_DEVELOPER_TEAM": teamID,
	}

	settings, ok := codesignAssetsByDistributionType[autocodesign.Development]
	if ok {
		outputs["BITRISE_DEVELOPMENT_CODESIGN_IDENTITY"] = settings.Certificate.CommonName

		bundleID, err := project.MainTargetBundleID()
		if err != nil {
			failf("Failed to read bundle ID for the main target: %s", err)
		}
		profile, ok := settings.ArchivableTargetProfilesByBundleID[bundleID]
		if !ok {
			failf("No provisioning profile ensured for the main target")
		}

		outputs["BITRISE_DEVELOPMENT_PROFILE"] = profile.Attributes().UUID
	}

	if distribution != autocodesign.Development {
		settings, ok := codesignAssetsByDistributionType[distribution]
		if !ok {
			failf("No codesign settings ensured for the selected distribution type: %s", distribution)
		}

		outputs["BITRISE_PRODUCTION_CODESIGN_IDENTITY"] = settings.Certificate.CommonName

		bundleID, err := project.MainTargetBundleID()
		if err != nil {
			failf(err.Error())
		}
		profile, ok := settings.ArchivableTargetProfilesByBundleID[bundleID]
		if !ok {
			failf("No provisioning profile ensured for the main target")
		}

		outputs["BITRISE_PRODUCTION_PROFILE"] = profile.Attributes().UUID
	}

	for k, v := range outputs {
		logger.Donef("%s=%s", k, v)
		if err := tools.ExportEnvironmentWithEnvman(k, v); err != nil {
			failf("Failed to export %s=%s: %s", k, v, err)
		}
	}
}
