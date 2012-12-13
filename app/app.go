// Copyright 2012 tsuru authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/globocom/config"
	"github.com/globocom/tsuru/api/auth"
	"github.com/globocom/tsuru/api/bind"
	"github.com/globocom/tsuru/api/service"
	"github.com/globocom/tsuru/db"
	"github.com/globocom/tsuru/log"
	"github.com/globocom/tsuru/provision/juju"
	"github.com/globocom/tsuru/queue"
	"github.com/globocom/tsuru/repository"
	"io"
	"labix.org/v2/mgo/bson"
	"launchpad.net/goyaml"
	"os"
	"os/exec"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
)

const RegenerateApprc = "regenerate-apprc"

func write(w io.Writer, content []byte) error {
	n, err := w.Write(content)
	if err != nil {
		return err
	}
	if n != len(content) {
		return io.ErrShortWrite
	}
	return nil
}

type App struct {
	Env       map[string]bind.EnvVar
	Framework string
	Logs      []Applog
	Name      string
	State     string
	Units     []Unit
	Teams     []string
	hooks     *conf
}

func (a *App) MarshalJSON() ([]byte, error) {
	result := make(map[string]interface{})
	result["Name"] = a.Name
	result["State"] = a.State
	result["Framework"] = a.Framework
	result["Teams"] = a.Teams
	result["Units"] = a.Units
	result["Repository"] = repository.GetUrl(a.Name)
	return json.Marshal(&result)
}

type Applog struct {
	Date    time.Time
	Message string
	Source  string
}

type conf struct {
	PreRestart []string `yaml:"pre-restart"`
	PosRestart []string `yaml:"pos-restart"`
}

func (a *App) Get() error {
	return db.Session.Apps().Find(bson.M{"name": a.Name}).One(a)
}

// CreateApp creates a new app.
//
// Creating a new app is a process composed of three steps:
//
//       1. Save the app in the database
//       2. Create S3 credentials and bucket for the app
//       3. Deploy juju charm
func CreateApp(a *App) error {
	if !a.isValid() {
		msg := "Invalid app name, your app should have at most 63 " +
			"characters, containing only lower case letters or numbers, " +
			"starting with a letter."
		return &ValidationError{Message: msg}
	}
	actions := []action{
		new(insertApp),
		new(createBucketIam),
		new(createRepository),
		new(provision),
	}
	return execute(a, actions)
}

// Deploys an app.
func (a *App) deploy() error {
	a.Log(fmt.Sprintf("creating app %s", a.Name), "tsuru")
	cmd := exec.Command("juju", "deploy", "--repository=/home/charms", "local:"+a.Framework, a.Name)
	log.Printf("deploying %s with name %s", a.Framework, a.Name)
	out, err := cmd.CombinedOutput()
	a.Log(string(out), "tsuru")
	return err
}

func (a *App) unbind() error {
	var instances []service.ServiceInstance
	err := db.Session.ServiceInstances().Find(bson.M{"apps": bson.M{"$in": []string{a.Name}}}).All(&instances)
	if err != nil {
		return err
	}
	var msg string
	var addMsg = func(instanceName string, reason error) {
		if msg == "" {
			msg = "Failed to unbind the following instances:\n"
		}
		msg += fmt.Sprintf("- %s (%s)", instanceName, reason.Error())
	}
	for _, instance := range instances {
		err = instance.Unbind(a)
		if err != nil {
			addMsg(instance.Name, err)
		}
	}
	if msg != "" {
		return errors.New(msg)
	}
	return nil
}

func (a *App) Destroy() error {
	err := destroyBucket(a)
	if err != nil {
		return err
	}
	if len(a.Units) > 0 {
		out, err := a.Unit().destroy()
		msg := fmt.Sprintf("Failed to destroy unit: %s\n%s", err, out)
		log.Print(msg)
		if err != nil {
			return errors.New(msg)
		}
		err = a.unbind()
		if err != nil {
			return err
		}
	}
	return db.Session.Apps().Remove(bson.M{"name": a.Name})
}

func (a *App) AddUnit(u *Unit) {
	for i, unt := range a.Units {
		if unt.Machine == u.Machine {
			a.Units[i] = *u
			return
		}
	}
	a.Units = append(a.Units, *u)
}

func (a *App) Find(team *auth.Team) (int, bool) {
	pos := sort.Search(len(a.Teams), func(i int) bool {
		return a.Teams[i] >= team.Name
	})
	return pos, pos < len(a.Teams) && a.Teams[pos] == team.Name
}

func (a *App) Grant(team *auth.Team) error {
	pos, found := a.Find(team)
	if found {
		return errors.New("This team already has access to this app")
	}
	a.Teams = append(a.Teams, "")
	tmp := a.Teams[pos]
	for i := pos; i < len(a.Teams)-1; i++ {
		a.Teams[i+1], tmp = tmp, a.Teams[i]
	}
	a.Teams[pos] = team.Name
	return nil
}

func (a *App) Revoke(team *auth.Team) error {
	index, found := a.Find(team)
	if !found {
		return errors.New("This team does not have access to this app")
	}
	copy(a.Teams[index:], a.Teams[index+1:])
	a.Teams = a.Teams[:len(a.Teams)-1]
	return nil
}

func (a *App) teams() []auth.Team {
	var teams []auth.Team
	db.Session.Teams().Find(bson.M{"_id": bson.M{"$in": a.Teams}}).All(&teams)
	return teams
}

func (a *App) SetTeams(teams []auth.Team) {
	a.Teams = make([]string, len(teams))
	for i, team := range teams {
		a.Teams[i] = team.Name
	}
	sort.Strings(a.Teams)
}

func (a *App) setEnv(env bind.EnvVar) {
	if a.Env == nil {
		a.Env = make(map[string]bind.EnvVar)
	}
	a.Env[env.Name] = env
	a.Log(fmt.Sprintf("setting env %s with value %s", env.Name, env.Value), "tsuru")
}

func (a *App) getEnv(name string) (bind.EnvVar, error) {
	var (
		env bind.EnvVar
		err error
		ok  bool
	)
	if env, ok = a.Env[name]; !ok {
		err = errors.New("Environment variable not declared for this app.")
	}
	return env, err
}

func (a *App) isValid() bool {
	regex := regexp.MustCompile(`^[a-z][a-z0-9]{0,62}$`)
	return regex.MatchString(a.Name)
}

func (a *App) InstanceEnv(name string) map[string]bind.EnvVar {
	envs := make(map[string]bind.EnvVar)
	for k, env := range a.Env {
		if env.InstanceName == name {
			envs[k] = bind.EnvVar(env)
		}
	}
	return envs
}

func deployHookAbsPath(p string) (string, error) {
	repoPath, err := config.GetString("git:unit-repo")
	if err != nil {
		return "", nil
	}
	cmdArgs := strings.Fields(p)
	abs := path.Join(repoPath, cmdArgs[0])
	_, err = os.Stat(abs)
	if os.IsNotExist(err) {
		return p, nil
	}
	cmdArgs[0] = abs
	return strings.Join(cmdArgs, " "), nil
}

// Loads restart hooks from app.conf.
func (a *App) loadHooks() error {
	if a.hooks != nil {
		return nil
	}
	a.hooks = new(conf)
	uRepo, err := repository.GetPath()
	if err != nil {
		a.Log(fmt.Sprintf("Got error while getting repository path: %s", err), "tsuru")
		return err
	}
	cmd := "cat " + path.Join(uRepo, "app.conf")
	var buf bytes.Buffer
	err = a.Unit().Command(&buf, &buf, cmd)
	if err != nil {
		a.Log(fmt.Sprintf("Got error while executing command: %s... Skipping hooks execution", err), "tsuru")
		return nil
	}
	err = goyaml.Unmarshal(juju.FilterOutput(buf.Bytes()), a.hooks)
	if err != nil {
		a.Log(fmt.Sprintf("Got error while parsing yaml: %s", err), "tsuru")
		return err
	}
	return nil
}

func (a *App) runHook(w io.Writer, cmds []string, kind string) error {
	if len(cmds) == 0 {
		a.Log(fmt.Sprintf("Skipping %s hooks...", kind), "tsuru")
		return nil
	}
	a.Log(fmt.Sprintf("Executing %s hook...", kind), "tsuru")
	err := write(w, []byte("\n ---> Running "+kind+"\n"))
	if err != nil {
		return err
	}
	for _, cmd := range cmds {
		p, err := deployHookAbsPath(cmd)
		if err != nil {
			a.Log(fmt.Sprintf("Error obtaining absolute path to hook: %s.", err), "tsuru")
			continue
		}
		err = a.Run(p, w)
		if err != nil {
			return err
		}
	}
	return err
}

// preRestart is responsible for running user's pre-restart script.
//
// The path to this script can be found at the app.conf file, at the root of user's app repository.
func (a *App) preRestart(w io.Writer) error {
	if err := a.loadHooks(); err != nil {
		return err
	}
	return a.runHook(w, a.hooks.PreRestart, "pre-restart")
}

// posRestart is responsible for running user's pos-restart script.
//
// The path to this script can be found at the app.conf file, at the root of
// user's app repository.
func (a *App) posRestart(w io.Writer) error {
	if err := a.loadHooks(); err != nil {
		return err
	}
	return a.runHook(w, a.hooks.PosRestart, "pos-restart")
}

// Run executes the command in app units
func (a *App) Run(cmd string, w io.Writer) error {
	a.Log(fmt.Sprintf("running '%s'", cmd), "tsuru")
	cmd = fmt.Sprintf("[ -f /home/application/apprc ] && source /home/application/apprc; [ -d /home/application/current ] && cd /home/application/current; %s", cmd)
	return a.Unit().Command(w, w, cmd)
}

// Restart runs the restart hook for the app
// and returns your output.
func Restart(a *App, w io.Writer) error {
	u := a.Unit()
	a.Log("executing hook to restart", "tsuru")
	err := a.preRestart(w)
	if err != nil {
		return err
	}
	err = write(w, []byte("\n ---> Restarting your app\n"))
	if err != nil {
		return err
	}
	err = a.posRestart(w)
	if err != nil {
		return err
	}
	return u.executeHook("restart", w)
}

// InstallDeps runs the dependencies hook for the app
// and returns your output.
func InstallDeps(a *App, w io.Writer) error {
	a.Log("executing hook dependencies", "tsuru")
	return a.Unit().executeHook("dependencies", w)
}

func (a *App) Unit() *Unit {
	if len(a.Units) > 0 {
		unit := a.Units[0]
		unit.app = a
		return &unit
	}
	return &Unit{app: a}
}

func (a *App) GetUnits() []bind.Unit {
	var units []bind.Unit
	for _, u := range a.Units {
		u.app = a
		units = append(units, &u)
	}
	return units
}

func (a *App) GetName() string {
	return a.Name
}

// SerializeEnvVars serializes the environment variables of the app. The
// environment variables will be written the the file /home/application/apprc
// in all units of the app.
//
// The wait parameter indicates whether it should wait or not for the write to
// complete.
func (a *App) SerializeEnvVars() {
	a.Unit().writeEnvVars()
}

func (a *App) SetEnvs(envs []bind.EnvVar, publicOnly bool) error {
	e := make([]bind.EnvVar, len(envs))
	for i, env := range envs {
		e[i] = bind.EnvVar(env)
	}
	return a.SetEnvsToApp(e, publicOnly, false)
}

func (a *App) enqueueApprcRegeneration() error {
	addr, err := config.GetString("queue-server")
	if err != nil {
		return err
	}
	messages, _, err := queue.Dial(addr)
	if err != nil {
		return err
	}
	messages <- queue.Message{
		Action: RegenerateApprc,
		Args:   []string{a.Name},
	}
	close(messages)
	return nil
}

// SetEnvsToApp adds environment variables to an app, serializing the resulting
// list of environment variables in all units of apps. This method can
// serialize them directly or using a queue.
//
// Besides the slice of environment variables, this method also takes two other
// parameters: publicOnly indicates whether only public variables can be
// overridden (if set to false, setEnvsToApp may override a private variable).
//
// If useQueue is true, it will use a queue to write the environment variables
// in the units of the app.
func (app *App) SetEnvsToApp(envs []bind.EnvVar, publicOnly, useQueue bool) error {
	if len(envs) > 0 {
		for _, env := range envs {
			set := true
			if publicOnly {
				e, err := app.getEnv(env.Name)
				if err == nil && !e.Public {
					set = false
				}
			}
			if set {
				app.setEnv(env)
			}
		}
		if err := db.Session.Apps().Update(bson.M{"name": app.Name}, app); err != nil {
			return err
		}
		if useQueue {
			return app.enqueueApprcRegeneration()
		}
		app.SerializeEnvVars()
	}
	return nil
}

func (a *App) UnsetEnvs(envs []string, publicOnly bool) error {
	return a.UnsetEnvsFromApp(envs, publicOnly, false)
}

// UnsetEnvsFromApp removes environment variables from an app, serializing the
// remaining list of environment variables to all units of the app. This method
// can serialize them directly or use a queue.
//
// Besides the slice with the name of the variables, this method also takes two
// other parameters: publicOnly indicates whether only public variables can be
// overridden (if set to false, setEnvsToApp may override a private variable).
//
// If useQueue is true, it will use a queue to write the environment variables
// in the units of the app.
func (app *App) UnsetEnvsFromApp(variableNames []string, publicOnly, useQueue bool) error {
	if len(variableNames) > 0 {
		for _, name := range variableNames {
			var unset bool
			e, err := app.getEnv(name)
			if !publicOnly || (err == nil && e.Public) {
				unset = true
			}
			if unset {
				delete(app.Env, name)
			}
		}
		if err := db.Session.Apps().Update(bson.M{"name": app.Name}, app); err != nil {
			return err
		}
		app.SerializeEnvVars()
	}
	return nil
}

func (a *App) Log(message string, source string) error {
	log.Printf(message)
	messages := strings.Split(message, "\n")
	for _, msg := range messages {
		filteredMessage := juju.FilterOutput([]byte(msg))
		if len(filteredMessage) > 0 {
			l := Applog{
				Date:    time.Now(),
				Message: msg,
				Source:  source,
			}
			a.Logs = append(a.Logs, l)
		}
	}
	return db.Session.Apps().Update(bson.M{"name": a.Name}, a)
}

type ValidationError struct {
	Message string
}

func (err *ValidationError) Error() string {
	return err.Message
}

func List(u *auth.User) ([]App, error) {
	var apps []App
	if u.IsAdmin() {
		if err := db.Session.Apps().Find(nil).All(&apps); err != nil {
			return []App{}, err
		}
		return apps, nil
	}
	ts, err := u.Teams()
	if err != nil {
		return []App{}, err
	}
	teams := auth.GetTeamsNames(ts)
	if err := db.Session.Apps().Find(bson.M{"teams": bson.M{"$in": teams}}).All(&apps); err != nil {
		return []App{}, err
	}
	return apps, nil
}
