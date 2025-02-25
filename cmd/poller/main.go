package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jenkins-x/go-scm/scm"
	"github.com/jenkins-x/lighthouse/pkg/filebrowser"
	gitv2 "github.com/jenkins-x/lighthouse/pkg/git/v2"
	"github.com/jenkins-x/lighthouse/pkg/poller"
	"github.com/pkg/errors"

	"github.com/jenkins-x/lighthouse/pkg/config"
	configutil "github.com/jenkins-x/lighthouse/pkg/config/util"
	"github.com/jenkins-x/lighthouse/pkg/interrupts"
	"github.com/jenkins-x/lighthouse/pkg/jobutil"
	"github.com/jenkins-x/lighthouse/pkg/logrusutil"
	"github.com/jenkins-x/lighthouse/pkg/util"
	"github.com/jenkins-x/lighthouse/pkg/watcher"
	"github.com/sirupsen/logrus"
)

type options struct {
	port                   int
	configPath             string
	jobConfigPath          string
	botName                string
	gitServerURL           string
	gitKind                string
	gitToken               string
	hmacToken              string
	namespace              string
	repoNames              string
	hookEndpoint           string
	runOnce                bool
	dryRun                 bool
	disablePollRelease     bool
	disablePollPullRequest bool
	pollPeriod             time.Duration
}

func (o *options) Validate() error {
	if o.hmacToken == "" {
		o.hmacToken = os.Getenv("HMAC_TOKEN")
	}
	return nil
}

func gatherOptions(fs *flag.FlagSet, args ...string) options {
	var o options
	fs.IntVar(&o.port, "port", 8888, "Port to listen on.")
	fs.StringVar(&o.configPath, "config-path", "", "Path to config.yaml.")
	fs.StringVar(&o.jobConfigPath, "job-config-path", "", "Path to prow job configs.")
	fs.StringVar(&o.botName, "bot-name", "", "The bot name")
	fs.StringVar(&o.gitServerURL, "git-url", "", "The git provider URL")
	fs.StringVar(&o.gitKind, "git-kind", "", "The git provider kind (e.g. github, gitlab, bitbucketserver")
	fs.BoolVar(&o.runOnce, "run-once", false, "If true, run only once then quit.")
	fs.BoolVar(&o.dryRun, "dry-run", false, "Disable POSTing to the webhook service and just log the webhooks instead.")
	fs.BoolVar(&o.disablePollRelease, "no-release", false, "Disable polling for new commits on the main branch (releases) - mostly used for easier testing/debugging.")
	fs.BoolVar(&o.disablePollPullRequest, "no-pr", false, "Disable polling for Pull Request changes - mostly used for easier testing/debugging.")

	fs.StringVar(&o.namespace, "namespace", "jx", "The namespace to listen in")
	fs.StringVar(&o.repoNames, "repo", "", "The git repository names to poll. If not specified all the repositories are polled")
	fs.StringVar(&o.hookEndpoint, "hook", os.Getenv("POLL_HOOK_ENDPOINT"), "The hook endpoint to post to")

	defaultPollPeriod := 20 * time.Second
	text := os.Getenv("POLL_PERIOD")
	if text != "" {
		d, err := time.ParseDuration(text)
		if err != nil {
			logrus.WithError(err).WithField("PollPeriod", text).Warn("invalid POLL_PERIOD value")
		} else {
			defaultPollPeriod = d
		}
	}
	fs.DurationVar(&o.pollPeriod, "period", defaultPollPeriod, "The time period between polls")

	err := fs.Parse(args)
	if err != nil {
		logrus.WithError(err).Fatal("Invalid options")
	}
	o.configPath = configutil.PathOrDefault(o.configPath)
	return o
}

func main() {
	logrusutil.ComponentInit("poller")

	defer interrupts.WaitForGracefulShutdown()

	jobutil.ServePProf()

	o := gatherOptions(flag.NewFlagSet(os.Args[0], flag.ExitOnError), os.Args[1:]...)
	if err := o.Validate(); err != nil {
		logrus.WithError(err).Fatal("Invalid options")
	}

	configAgent := &config.Agent{}
	cfgMapWatcher, err := watcher.SetupConfigMapWatchers(o.namespace, configAgent, nil)
	if err != nil {
		logrus.WithError(err).Fatal("error starting config map watcher")
	}
	defer cfgMapWatcher.Stop()

	botName := o.botName
	if botName == "" {
		botName = util.GetBotName(configAgent.Config)
	}
	if util.GetGitHubAppSecretDir() != "" {
		botName, err = util.GetGitHubAppAPIUser()
		if err != nil {
			logrus.WithError(err).Fatal("unable to read API user for GitHub App integration")
		}
	}
	if botName == "" {
		logrus.Fatal("no $GIT_USER defined")
	}
	if o.hookEndpoint == "" {
		logrus.Fatal("no hook endpoint defined")
	}
	serverURL := o.gitServerURL
	if serverURL == "" {
		serverURL = util.GetGitServer(configAgent.Config)
	}
	gitKind := o.gitKind
	if gitKind == "" {
		gitKind = util.GitKind(configAgent.Config)
	}
	o.gitToken, err = util.GetSCMToken(gitKind)
	if err != nil {
		logrus.WithError(err).Fatal("Error creating Poller controller.")
	}
	if o.gitToken == "" {
		logrus.WithError(err).Fatal("no git token.")
	}

	gitCloneUser := os.Getenv("GIT_USER")
	if gitCloneUser == "" {
		gitCloneUser = os.Getenv("GIT_USERNAME")
	}
	if gitCloneUser == "" {
		gitCloneUser = o.botName
	}
	u, err := url.Parse(serverURL)
	if err != nil {
		logrus.WithError(err).Fatalf("failed to parse git server %s", serverURL)
	}

	configureOpts := func(opts *gitv2.ClientFactoryOpts) {
		opts.Token = func() []byte {
			return []byte(o.gitToken)
		}
		opts.GitUser = func() (name, email string, err error) {
			name = gitCloneUser
			return
		}
		opts.Username = func() (login string, err error) {
			login = gitCloneUser
			return
		}
		opts.Host = u.Host
		opts.Scheme = u.Scheme
		opts.UseUserInURL = true
	}
	gitFactory, err := gitv2.NewNoMirrorClientFactory(configureOpts)
	if err != nil {
		logrus.WithError(err).Fatalf("failed to create git client factory for server %s", o.gitServerURL)
	}
	fb := filebrowser.NewFileBrowserFromGitClient(gitFactory)

	var repoNames []string
	if o.repoNames != "" {
		repoNames = strings.Split(o.repoNames, ",")
	}
	if len(repoNames) == 0 {
		cfg := configAgent.Config
		repoNames = findAllRepoNames(cfg())
	}
	if len(repoNames) == 0 {
		logrus.Fatal("no repositories found")
	}

	gitHubAppOwner := ""
	_, scmClient, _, _, err := util.GetSCMClient(gitHubAppOwner, configAgent.Config)
	if err != nil {
		logrus.WithError(err).Fatal("failed to create scm client")
	}

	c, err := poller.NewPollingController(repoNames, serverURL, scmClient, fb, o.notifier)
	if err != nil {
		logrus.WithError(err).Fatal("Error creating Poller controller.")
	}

	c.DisablePollPullRequest = o.disablePollPullRequest
	c.DisablePollRelease = o.disablePollRelease

	c.Logger().WithFields(map[string]interface{}{
		"PollPeriod":             o.pollPeriod,
		"HookEndpint":            o.hookEndpoint,
		"DisablePollRelease":     o.disablePollRelease,
		"DisablePollPullRequest": o.disablePollPullRequest,
	}).Info("starting")

	http.Handle("/", c)
	server := &http.Server{Addr: ":" + strconv.Itoa(o.port)}

	if o.runOnce {
		c.Sync()
		return
	}

	interrupts.Tick(func() {
		c.Sync()
	}, func() time.Duration {
		return o.pollPeriod
	})

	// serve data
	logrus.WithField("port", o.port).Info("Starting HTTP server")
	interrupts.ListenAndServe(server, 10*time.Second)

	interrupts.WaitForGracefulShutdown()
}

func findAllRepoNames(c *config.Config) []string {
	m := map[string]bool{}

	for fullName := range c.Presubmits {
		m[fullName] = true
	}
	for fullName := range c.Postsubmits {
		m[fullName] = true
	}
	for fullName := range c.InRepoConfig.Enabled {
		if !strings.Contains(fullName, "*") && strings.Contains(fullName, "/") {
			m[fullName] = true
		}
	}
	var repoNames []string
	for fullName := range m {
		repoNames = append(repoNames, fullName)
	}
	return repoNames
}

func (o *options) notifier(hook *scm.WebhookWrapper) error {
	if o.dryRun {
		logrus.WithField("Hook", hook).Info("notify")
		return nil
	}

	data, err := json.Marshal(hook)
	if err != nil {
		return errors.Wrapf(err, "failed to marshal hook %#v", hook)
	}

	req, err := http.NewRequest("POST", o.hookEndpoint, bytes.NewBuffer(data))
	req.Header.Set("Content-Type", "application/json")

	if o.hmacToken != "" {
		sig := util.CreateHMACHeader(data, o.hmacToken)
		req.Header.Set("X-Hub-Signature", sig)
	}

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		if err != nil {
			return errors.Wrapf(err, "failed to invoke endpoint %s", o.hookEndpoint)
		}
	}
	defer resp.Body.Close()

	body, _ := ioutil.ReadAll(resp.Body)
	l := logrus.WithFields(map[string]interface{}{
		"Status":   resp.Status,
		"Endpoint": o.hookEndpoint,
		"Body":     string(body),
	})
	if resp.StatusCode >= 300 {
		l.Warnf("failed to notify")
		return errors.Errorf("status when invoking endpoint")
	}
	l.Infof("notified")
	return nil
}
