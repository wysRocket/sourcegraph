package graphqlbackend

import (
	"context"
	"fmt"
	"net/smtp"
	"strconv"
	"strings"
	"time"

	"github.com/Masterminds/semver"
	"github.com/gomodule/redigo/redis"
	"github.com/inconshreveable/log15"

	"github.com/sourcegraph/sourcegraph/cmd/frontend/hooks"
	"github.com/sourcegraph/sourcegraph/internal/actor"
	"github.com/sourcegraph/sourcegraph/internal/auth"
	"github.com/sourcegraph/sourcegraph/internal/codygateway"
	"github.com/sourcegraph/sourcegraph/internal/conf"
	"github.com/sourcegraph/sourcegraph/internal/conf/conftypes"
	"github.com/sourcegraph/sourcegraph/internal/conf/deploy"
	"github.com/sourcegraph/sourcegraph/internal/database"
	"github.com/sourcegraph/sourcegraph/internal/env"
	"github.com/sourcegraph/sourcegraph/internal/extsvc"
	"github.com/sourcegraph/sourcegraph/internal/extsvc/versions"
	"github.com/sourcegraph/sourcegraph/internal/featureflag"
	"github.com/sourcegraph/sourcegraph/internal/licensing"
	"github.com/sourcegraph/sourcegraph/internal/redispool"
	"github.com/sourcegraph/sourcegraph/internal/settings"
	srcprometheus "github.com/sourcegraph/sourcegraph/internal/src-prometheus"
	"github.com/sourcegraph/sourcegraph/internal/txemail"
	"github.com/sourcegraph/sourcegraph/internal/updatecheck"
	"github.com/sourcegraph/sourcegraph/internal/version"
	"github.com/sourcegraph/sourcegraph/lib/errors"
	"github.com/sourcegraph/sourcegraph/schema"
)

type AlertGroup string

var (
	AlertGroupAuthentication AlertGroup = "AUTHENTICATION"
	AlertGroupCodehost       AlertGroup = "CODEHOST"
	AlertGroupCody           AlertGroup = "CODY"
	AlertGroupLicense        AlertGroup = "LICENSE"
	AlertGroupObservability  AlertGroup = "OBSERVABILITY"
	AlertGroupSMTP           AlertGroup = "SMTP"
	AlertGroupUpdate         AlertGroup = "UPDATE"
	AlertGroupOther          AlertGroup = "OTHER"
)

type AlertType string

// Alert implements the GraphQL type Alert.
type Alert struct {
	GroupValue                AlertGroup
	TypeValue                 AlertType
	MessageValue              string
	IsDismissibleWithKeyValue string
}

func (r *Alert) Group() string   { return string(r.GroupValue) }
func (r *Alert) Type() string    { return string(r.TypeValue) }
func (r *Alert) Message() string { return r.MessageValue }
func (r *Alert) IsDismissibleWithKey() *string {
	if r.IsDismissibleWithKeyValue == "" {
		return nil
	}
	return &r.IsDismissibleWithKeyValue
}

// Constants for the GraphQL enum AlertType.
const (
	AlertTypeInfo    AlertType = "INFO"
	AlertTypeWarning AlertType = "WARNING"
	AlertTypeError   AlertType = "ERROR"
)

// AlertFuncs is a list of functions called to populate the GraphQL Site.alerts value. It may be
// appended to at init time.
//
// The functions are called each time the Site.alerts value is queried, so they must not block.
var AlertFuncs []func(AlertFuncArgs) []*Alert

// AlertFuncArgs are the arguments provided to functions in AlertFuncs used to populate the GraphQL
// Site.alerts value. They allow the functions to customize the returned alerts based on the
// identity of the viewer (without needing to query for that on their own, which would be slow).
type AlertFuncArgs struct {
	IsAuthenticated     bool             // whether the viewer is authenticated
	IsSiteAdmin         bool             // whether the viewer is a site admin
	ViewerFinalSettings *schema.Settings // the viewer's final user/org/global settings
	Ctx                 context.Context
	DB                  database.DB
}

func (r *siteResolver) Alerts(ctx context.Context) ([]*Alert, error) {
	settings, err := settings.CurrentUserFinal(ctx, r.db)
	if err != nil {
		return nil, err
	}

	args := AlertFuncArgs{
		IsAuthenticated:     actor.FromContext(ctx).IsAuthenticated(),
		IsSiteAdmin:         auth.CheckCurrentUserIsSiteAdmin(ctx, r.db) == nil,
		ViewerFinalSettings: settings,
		Ctx:                 ctx,
		DB:                  r.db,
	}

	var alerts []*Alert
	for _, f := range AlertFuncs {
		alerts = append(alerts, f(args)...)
	}
	return alerts, nil
}

// Intentionally named "DISABLE_SECURITY" and not something else, so that anyone considering
// disabling this thinks twice about the risks associated with disabling these and considers
// keeping up-to-date more frequently instead.
var disableSecurity, _ = strconv.ParseBool(env.Get("DISABLE_SECURITY", "false", "disables security upgrade notices"))

func init() {
	conf.ContributeWarning(func(c conftypes.SiteConfigQuerier) (problems conf.Problems) {
		if deploy.IsDeployTypeSingleDockerContainer(deploy.Type()) || deploy.IsApp() {
			return nil
		}
		if c.SiteConfig().ExternalURL == "" {
			problems = append(problems, conf.NewSiteProblem("`externalURL` is required to be set for many features of Sourcegraph to work correctly."))
		}
		return problems
	})

	if !disableSecurity {
		// Warn about Sourcegraph being out of date.
		AlertFuncs = append(AlertFuncs, outOfDateAlert)
	} else {
		log15.Warn("WARNING: SECURITY NOTICES DISABLED: this is not recommended, please unset DISABLE_SECURITY=true")
	}

	// Notify when updates are available, if the instance can access the public internet.
	AlertFuncs = append(AlertFuncs, updateAvailableAlert)

	AlertFuncs = append(AlertFuncs, storageLimitReachedAlert)

	AlertFuncs = append(AlertFuncs, freePlanAlert)

	AlertFuncs = append(AlertFuncs, userCountExceededAlert)

	// handle alerts for SMTP configuration problems
	AlertFuncs = append(AlertFuncs, smtpConfigAlert(txemail.CreateSMTPClient))

	// Notify admins if critical alerts are firing, if Prometheus is configured.
	prom, err := srcprometheus.NewClient(srcprometheus.PrometheusURL)
	if err == nil {
		AlertFuncs = append(AlertFuncs, observabilityActiveAlertsAlert(prom))
	} else if !errors.Is(err, srcprometheus.ErrPrometheusUnavailable) {
		log15.Warn("WARNING: possibly misconfigured Prometheus", "error", err)
	}

	AlertFuncs = append(AlertFuncs, func(args AlertFuncArgs) []*Alert {
		var alerts []*Alert
		for _, notification := range conf.Get().Notifications {
			alerts = append(alerts, &Alert{
				GroupValue:                AlertGroupOther,
				TypeValue:                 AlertTypeInfo,
				MessageValue:              notification.Message,
				IsDismissibleWithKeyValue: notification.Key,
			})
		}
		return alerts
	})

	// Warn about invalid site configuration.
	AlertFuncs = append(AlertFuncs, func(args AlertFuncArgs) []*Alert {
		// 🚨 SECURITY: Only the site admin should care about the site configuration being invalid, as they
		// are the only one who can take action on that. Additionally, it may be unsafe to expose information
		// about the problems with the configuration (e.g. if the error message contains sensitive information).
		if !args.IsSiteAdmin {
			return nil
		}

		problems, err := conf.Validate(conf.Raw())
		if err != nil {
			return []*Alert{
				{
					GroupValue:   AlertGroupOther,
					TypeValue:    AlertTypeError,
					MessageValue: `Update [**site configuration**](/site-admin/configuration) to resolve problems: ` + err.Error(),
				},
			}
		}

		warnings, err := conf.GetWarnings()
		if err != nil {
			return []*Alert{
				{
					GroupValue:   AlertGroupOther,
					TypeValue:    AlertTypeError,
					MessageValue: `Update [**site configuration**](/site-admin/configuration) to resolve problems: ` + err.Error(),
				},
			}
		}
		problems = append(problems, warnings...)

		if len(problems) == 0 {
			return nil
		}
		alerts := make([]*Alert, 0, 2)

		siteProblems := problems.Site()
		if len(siteProblems) > 0 {
			alerts = append(alerts, &Alert{
				GroupValue: AlertGroupOther,
				TypeValue:  AlertTypeWarning,
				MessageValue: `[**Update site configuration**](/site-admin/configuration) to resolve problems:` +
					"\n* " + strings.Join(siteProblems.Messages(), "\n* "),
			})
		}

		externalServiceProblems := problems.ExternalService()
		if len(externalServiceProblems) > 0 {
			alerts = append(alerts, &Alert{
				GroupValue: AlertGroupCodehost,
				TypeValue:  AlertTypeWarning,
				MessageValue: `[**Update external service configuration**](/site-admin/external-services) to resolve problems:` +
					"\n* " + strings.Join(externalServiceProblems.Messages(), "\n* "),
			})
		}
		return alerts
	})

	// Warn if customer is using GitLab on a version < 12.0.
	AlertFuncs = append(AlertFuncs, gitlabVersionAlert)

	AlertFuncs = append(AlertFuncs, codyGatewayUsageAlert)
}

func freePlanAlert(args AlertFuncArgs) []*Alert {
	// We only show this alert to site admins.
	if !args.IsSiteAdmin {
		return nil
	}

	if !featureflag.FromContext(args.Ctx).GetBoolOr("setup-checklist", false) {
		log15.Warn("Setup checklist feature flag is disabled, skipping free plan alert.")
		return nil
	}
	info, err := licensing.GetConfiguredProductLicenseInfo()
	if err != nil {
		log15.Error("Error getting configured product license info", "error", err)
		return nil
	}

	if info == nil {
		log15.Error("No license info available")
		return nil
	}

	plan := info.Plan()
	if plan == licensing.PlanFree1 || plan == licensing.PlanFree0 {
		return []*Alert{{
			GroupValue:                AlertGroupLicense,
			TypeValue:                 AlertTypeWarning,
			MessageValue:              "You're on a free Sourcegraph plan. [Upgrade](https://about.sourcegraph.com/pricing) to unlock more features and support. [Set license key](/site-admin/configuration)",
			IsDismissibleWithKeyValue: "free-plan-upgrade",
		}}
	}
	return nil
}

func userCountExceededAlert(args AlertFuncArgs) []*Alert {
	// We only show this alert to site admins.
	if !args.IsSiteAdmin {
		return nil
	}

	if !featureflag.FromContext(args.Ctx).GetBoolOr("setup-checklist", false) {
		return nil
	}

	info, err := licensing.GetConfiguredProductLicenseInfo()
	if err != nil {
		log15.Error("Error getting configured product license info", "error", err)
		return nil
	}

	if info == nil {
		log15.Error("No license info available")
		return nil
	}

	if info.HasTag(licensing.TrueUpUserCountTag) {
		return nil
	}

	var licensedUserCount int32
	if info != nil {
		licensedUserCount = int32(info.UserCount)
	} else {
		licensedUserCount = licensing.NoLicenseMaximumAllowedUserCount
	}

	userCount, err := args.DB.Users().Count(args.Ctx, nil)
	if err != nil {
		log15.Error("Error counting users", "error", err)
		return nil
	}

	if licensedUserCount > 0 && int32(userCount) >= licensedUserCount {
		return []*Alert{{
			GroupValue:                AlertGroupLicense,
			TypeValue:                 AlertTypeWarning,
			MessageValue:              fmt.Sprintf("You have reached the maximum user count (%d) for your current Sourcegraph license. [Upgrade](https://about.sourcegraph.com/pricing) to support more users.", licensedUserCount),
			IsDismissibleWithKeyValue: "user-count-limit",
		}}
	}

	return nil
}

func smtpConfigAlert(createClient func(schema.SiteConfiguration) (*smtp.Client, error)) func(args AlertFuncArgs) []*Alert {
	return func(args AlertFuncArgs) []*Alert {
		// We only show this alert to site admins.
		if !args.IsSiteAdmin || deploy.IsDeployTypeSingleDockerContainer(deploy.Type()) || deploy.IsApp() {
			return nil
		}

		cfg := conf.SiteConfig()
		hasSMTP := cfg.EmailSmtp != nil && cfg.EmailSmtp.Host != ""

		alerts := []*Alert{}

		if !hasSMTP || cfg.EmailAddress == "" {
			alerts = append(alerts, &Alert{
				GroupValue:                AlertGroupSMTP,
				TypeValue:                 AlertTypeWarning,
				MessageValue:              "SMTP is not configured. Email notifications will not be sent. [Configure SMTP](/site-admin/configuration#smtp) or [see the docs](/help/admin/config/email).",
				IsDismissibleWithKeyValue: "smtp-config-missing",
			})
		}

		hasSMTPAuth := hasSMTP && cfg.EmailSmtp.Authentication != "none"
		if hasSMTPAuth && (cfg.EmailSmtp.Username == "" || cfg.EmailSmtp.Password == "") {
			alerts = append(alerts, &Alert{
				GroupValue:                AlertGroupSMTP,
				TypeValue:                 AlertTypeError,
				MessageValue:              fmt.Sprintf("SMTP authentication is misconfigured. SMTP Authentication is set to %s, but username or password is missing. [Configure SMTP](/site-admin/configuration#smtp) or [see the docs](/help/admin/config/email).", cfg.EmailSmtp.Authentication),
				IsDismissibleWithKeyValue: "smtp-config-auth-error",
			})
		}
		// return early, errors above block the smtp client from being created correctly
		if len(alerts) > 0 {
			return alerts
		}

		client, err := createClient(cfg)
		if err != nil {
			return []*Alert{{
				GroupValue:                AlertGroupSMTP,
				TypeValue:                 AlertTypeError,
				MessageValue:              "SMTP server cannot be reached, please check your SMTP configuration. [Configure SMTP](/site-admin/configuration#smtp) or [see the docs](/help/admin/config/email).",
				IsDismissibleWithKeyValue: "smtp-client-error",
			}}
		}
		defer func() {
			if client.Text != nil {
				_ = client.Close()
			}
		}()

		return nil
	}
}

func storageLimitReachedAlert(args AlertFuncArgs) []*Alert {
	licenseInfo := hooks.GetLicenseInfo()
	if licenseInfo == nil {
		return nil
	}

	if licenseInfo.CodeScaleCloseToLimit {
		return []*Alert{{
			GroupValue:   AlertGroupLicense,
			TypeValue:    AlertTypeWarning,
			MessageValue: "You're about to reach the 100GiB storage limit. Upgrade to [Sourcegraph Enterprise](https://about.sourcegraph.com/pricing) for unlimited storage for your code.",
		}}
	} else if licenseInfo.CodeScaleExceededLimit {
		return []*Alert{{
			GroupValue:   AlertGroupLicense,
			TypeValue:    AlertTypeError,
			MessageValue: "You've used all 100GiB of storage. Upgrade to [Sourcegraph Enterprise](https://about.sourcegraph.com/pricing) for unlimited storage for your code.",
		}}
	}
	return nil
}

func updateAvailableAlert(args AlertFuncArgs) []*Alert {
	// TODO(app): App Update Messaging
	// See: https://github.com/sourcegraph/sourcegraph/issues/48851
	if deploy.IsApp() {
		return nil
	}

	// We only show update alerts to admins. This is not for security reasons, as we already
	// expose the version number of the instance to all users via the user settings page.
	if !args.IsSiteAdmin {
		return nil
	}

	globalUpdateStatus := updatecheck.Last()
	if globalUpdateStatus == nil || updatecheck.IsPending() || !globalUpdateStatus.HasUpdate() || globalUpdateStatus.Err != nil {
		return nil
	}
	// ensure the user has opted in to receiving notifications for minor updates and there is one available
	if !args.ViewerFinalSettings.AlertsShowPatchUpdates && !isMinorUpdateAvailable(version.Version(), globalUpdateStatus.UpdateVersion) {
		return nil
	}
	// for major/minor updates, ensure banner is hidden if they have opted out
	if !args.ViewerFinalSettings.AlertsShowMajorMinorUpdates && isMinorUpdateAvailable(version.Version(), globalUpdateStatus.UpdateVersion) {
		return nil
	}
	message := fmt.Sprintf("An update is available: [Sourcegraph v%s](https://about.sourcegraph.com/blog) - [changelog](https://about.sourcegraph.com/changelog)", globalUpdateStatus.UpdateVersion)

	// dismission key includes the version so after it is dismissed the alert comes back for the next update.
	key := "update-available-" + globalUpdateStatus.UpdateVersion
	return []*Alert{{
		GroupValue: AlertGroupOther,
		TypeValue:  AlertTypeInfo, MessageValue: message, IsDismissibleWithKeyValue: key,
	}}
}

// isMinorUpdateAvailable tells if upgrading from the current version to the specified upgrade
// candidate would be a major/minor update and NOT a patch update.
func isMinorUpdateAvailable(currentVersion, updateVersion string) bool {
	// If either current or update versions aren't semvers (e.g., a user is on a date-based build version, or "dev"),
	// always return true and allow any alerts to be shown. This has the effect of simply deferring to the response
	// from Sourcegraph.com about whether an update alert is needed.
	cv, err := semver.NewVersion(currentVersion)
	if err != nil {
		return true
	}
	uv, err := semver.NewVersion(updateVersion)
	if err != nil {
		return true
	}
	return cv.Major() != uv.Major() || cv.Minor() != uv.Minor()
}

func outOfDateAlert(args AlertFuncArgs) []*Alert {
	// TODO(app): App Update Messaging
	// See: https://github.com/sourcegraph/sourcegraph/issues/48851
	if deploy.IsApp() {
		return nil
	}

	globalUpdateStatus := updatecheck.Last()
	if globalUpdateStatus == nil || updatecheck.IsPending() {
		return nil
	}
	offline := globalUpdateStatus.Err != nil // Whether or not instance can connect to Sourcegraph.com for update checks
	now := time.Now()
	monthsOutOfDate, err := version.HowLongOutOfDate(now)
	if err != nil {
		log15.Error("failed to determine how out of date Sourcegraph is", "error", err)
		return nil
	}
	alert := determineOutOfDateAlert(args.IsSiteAdmin, monthsOutOfDate, offline)
	if alert == nil {
		return nil
	}
	return []*Alert{alert}
}

func determineOutOfDateAlert(isAdmin bool, months int, offline bool) *Alert {
	if months <= 0 {
		return nil
	}
	// online instances will still be prompt site admins to upgrade via site_update_check
	if months < 3 && !offline {
		return nil
	}

	if isAdmin {
		key := fmt.Sprintf("months-out-of-date-%d", months)
		switch {
		case months < 3:
			message := fmt.Sprintf("Sourcegraph is %d+ months out of date, for the latest features and bug fixes please upgrade ([changelog](http://about.sourcegraph.com/changelog))", months)
			return &Alert{GroupValue: AlertGroupUpdate, TypeValue: AlertTypeInfo, MessageValue: message, IsDismissibleWithKeyValue: key}
		case months == 3:
			message := "Sourcegraph is 3+ months out of date, you may be missing important security or bug fixes. Users will be notified at 4+ months. ([changelog](http://about.sourcegraph.com/changelog))"
			return &Alert{GroupValue: AlertGroupUpdate, TypeValue: AlertTypeWarning, MessageValue: message}
		case months == 4:
			message := "Sourcegraph is 4+ months out of date, you may be missing important security or bug fixes. A notice is shown to users. ([changelog](http://about.sourcegraph.com/changelog))"
			return &Alert{GroupValue: AlertGroupUpdate, TypeValue: AlertTypeWarning, MessageValue: message}
		case months == 5:
			message := "Sourcegraph is 5+ months out of date, you may be missing important security or bug fixes. A notice is shown to users. ([changelog](http://about.sourcegraph.com/changelog))"
			return &Alert{GroupValue: AlertGroupUpdate, TypeValue: AlertTypeError, MessageValue: message}
		default:
			message := fmt.Sprintf("Sourcegraph is %d+ months out of date, you may be missing important security or bug fixes. A notice is shown to users. ([changelog](http://about.sourcegraph.com/changelog))", months)
			return &Alert{GroupValue: AlertGroupUpdate, TypeValue: AlertTypeError, MessageValue: message}
		}
	}

	key := fmt.Sprintf("months-out-of-date-%d", months)
	switch months {
	case 0, 1, 2, 3:
		return nil
	case 4, 5:
		message := fmt.Sprintf("Sourcegraph is %d+ months out of date, ask your site administrator to upgrade for the latest features and bug fixes. ([changelog](http://about.sourcegraph.com/changelog))", months)
		return &Alert{GroupValue: AlertGroupUpdate, TypeValue: AlertTypeWarning, MessageValue: message, IsDismissibleWithKeyValue: key}
	default:
		alertType := AlertTypeWarning
		if months > 12 {
			alertType = AlertTypeError
		}
		message := fmt.Sprintf("Sourcegraph is %d+ months out of date, you may be missing important security or bug fixes. Ask your site administrator to upgrade. ([changelog](http://about.sourcegraph.com/changelog))", months)
		return &Alert{GroupValue: AlertGroupUpdate, TypeValue: alertType, MessageValue: message, IsDismissibleWithKeyValue: key}
	}
}

// observabilityActiveAlertsAlert directs admins to check Grafana if critical alerts are firing
func observabilityActiveAlertsAlert(prom srcprometheus.Client) func(AlertFuncArgs) []*Alert {
	return func(args AlertFuncArgs) []*Alert {
		// true by default - change settings.schema.json if this changes
		// blocked by https://github.com/sourcegraph/sourcegraph/issues/12190
		observabilitySiteAlertsDisabled := true
		if args.ViewerFinalSettings != nil && args.ViewerFinalSettings.AlertsHideObservabilitySiteAlerts != nil {
			observabilitySiteAlertsDisabled = *args.ViewerFinalSettings.AlertsHideObservabilitySiteAlerts
		}

		if !args.IsSiteAdmin || observabilitySiteAlertsDisabled {
			return nil
		}

		// use a short timeout to avoid having this block problems from loading
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		status, err := prom.GetAlertsStatus(ctx)
		if err != nil {
			return []*Alert{{GroupValue: AlertGroupObservability, TypeValue: AlertTypeWarning, MessageValue: fmt.Sprintf("Failed to fetch alerts status: %s", err)}}
		}

		// decide whether to render a message about alerts
		if status.Critical == 0 {
			return nil
		}
		msg := fmt.Sprintf("%s across %s currently firing - [view alerts](/-/debug/grafana)",
			pluralize(status.Critical, "critical alert", "critical alerts"),
			pluralize(status.ServicesCritical, "service", "services"))
		return []*Alert{{GroupValue: AlertGroupObservability, TypeValue: AlertTypeError, MessageValue: msg}}
	}
}

func gitlabVersionAlert(args AlertFuncArgs) []*Alert {
	// We only show this alert to site admins.
	if !args.IsSiteAdmin {
		return nil
	}

	chvs, err := versions.GetVersions()
	if err != nil {
		log15.Warn("Failed to get code host versions for GitLab minimum version alert", "error", err)
		return nil
	}

	// NOTE: It's necessary to include a "-0" prerelease suffix on each constraint so that
	// prereleases of future versions are still considered to satisfy the constraint. See
	// https://github.com/Masterminds/semver#working-with-prerelease-versions for more.
	mv, err := semver.NewConstraint(">=12.0.0-0")
	if err != nil {
		log15.Warn("Failed to create minimum version constraint for GitLab minimum version alert", "error", err)
	}

	for _, chv := range chvs {
		if chv.ExternalServiceKind != extsvc.KindGitLab {
			continue
		}

		cv, err := semver.NewVersion(chv.Version)
		if err != nil {
			log15.Warn("Failed to parse code host version for GitLab minimum version alert", "error", err, "external_service_kind", chv.ExternalServiceKind)
			continue
		}

		if !mv.Check(cv) {
			log15.Debug("Detected GitLab instance running a version below 12.0.0", "version", chv.Version)

			return []*Alert{{
				GroupValue:   AlertGroupCodehost,
				TypeValue:    AlertTypeError,
				MessageValue: "One or more of your code hosts is running a version of GitLab below 12.0, which is not supported by Sourcegraph. Please upgrade your GitLab instance(s) to prevent disruption.",
			}}
		}
	}

	return nil
}

func codyGatewayUsageAlert(args AlertFuncArgs) []*Alert {
	// We only show this alert to site admins.
	if !args.IsSiteAdmin {
		return nil
	}

	var alerts []*Alert

	for _, feat := range codygateway.AllFeatures {
		val := redispool.Store.Get(fmt.Sprintf("%s:%s", codygateway.CodyGatewayUsageRedisKeyPrefix, string(feat)))
		usage, err := val.Int()
		if err != nil {
			if err == redis.ErrNil {
				continue
			}
			log15.Warn("Failed to read Cody Gateway usage for feature", "feature", feat)
			continue
		}
		if usage > 99 {
			alerts = append(alerts, &Alert{
				GroupValue:   AlertGroupCody,
				TypeValue:    AlertTypeError,
				MessageValue: fmt.Sprintf("The Cody limit for %s has been reached. If you run into this regularly, please contact Sourcegraph.", feat.DisplayName()),
			})
		} else if usage >= 90 {
			alerts = append(alerts, &Alert{
				GroupValue:   AlertGroupCody,
				TypeValue:    AlertTypeWarning,
				MessageValue: fmt.Sprintf("The Cody limit for %s is 90%% used. If you run into this regularly, please contact Sourcegraph.", feat.DisplayName()),
			})
		} else if usage >= 75 {
			alerts = append(alerts, &Alert{
				GroupValue:   AlertGroupCody,
				TypeValue:    AlertTypeInfo,
				MessageValue: fmt.Sprintf("The Cody limit for %s is 75%% used. If you run into this regularly, please contact Sourcegraph.", feat.DisplayName()),
			})
		}
	}

	return alerts
}

func pluralize(v int, singular, plural string) string {
	if v == 1 {
		return fmt.Sprintf("%d %s", v, singular)
	}
	return fmt.Sprintf("%d %s", v, plural)
}
