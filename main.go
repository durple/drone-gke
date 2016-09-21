package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/drone/drone-plugin-go/plugin"
)

type GKE struct {
	DryRun         bool                   `json:"dry_run"`
	Verbose        bool                   `json:"verbose"`
	Token          string                 `json:"token"`
	GCloudCmd      string                 `json:"gcloud_cmd"`
	KubectlCmd     string                 `json:"kubectl_cmd"`
	Project        string                 `json:"project"`
	Zone           string                 `json:"zone"`
	Cluster        string                 `json:"cluster"`
	Template       string                 `json:"template"`
	SecretTemplate string                 `json:"secret_template"`
	Vars           map[string]interface{} `json:"vars"`
	Secrets        map[string]interface{} `json:"secrets"`
}

var (
	rev string
)

func main() {
	err := wrapMain()
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

func wrapMain() error {
	if rev == "" {
		rev = "[unknown]"
	}

	fmt.Printf("Drone GKE Plugin built from %s\n", rev)

	// https://godoc.org/github.com/drone/drone-plugin-go/plugin
	workspace := plugin.Workspace{}
	repo := plugin.Repo{}
	build := plugin.Build{}
	system := plugin.System{}
	vargs := GKE{}

	plugin.Param("workspace", &workspace)
	plugin.Param("repo", &repo)
	plugin.Param("build", &build)
	plugin.Param("system", &system)
	plugin.Param("vargs", &vargs)
	plugin.MustParse()

	// Check required params

	if vargs.Token == "" {
		return fmt.Errorf("missing required param: token")
	}

	if vargs.Project == "" {
		vargs.Project = getProjectFromToken(vargs.Token)
	}

	if vargs.Project == "" {
		return fmt.Errorf("missing required param: project")
	}

	if vargs.Zone == "" {
		return fmt.Errorf("missing required param: zone")
	}

	sdkPath := "/google-cloud-sdk"
	keyPath := "/tmp/gcloud.json"

	// Defaults

	if vargs.GCloudCmd == "" {
		vargs.GCloudCmd = fmt.Sprintf("%s/bin/gcloud", sdkPath)
	}

	if vargs.KubectlCmd == "" {
		vargs.KubectlCmd = fmt.Sprintf("%s/bin/kubectl", sdkPath)
	}

	// Trim whitespace, to forgive the vagaries of YAML parsing.
	vargs.Token = strings.TrimSpace(vargs.Token)

	// Write credentials to tmp file to be picked up by the 'gcloud' command.
	// This is inside the ephemeral plugin container, not on the host.
	err := ioutil.WriteFile(keyPath, []byte(vargs.Token), 0600)
	if err != nil {
		return fmt.Errorf("error writing token file: %s\n", err)
	}

	// Warn if the keyfile can't be deleted, but don't abort. We're almost
	// certainly running inside an ephemeral container, so the file will be
	// discarded when we're finished anyway.
	defer func() {
		err := os.Remove(keyPath)
		if err != nil {
			fmt.Printf("warning: error removing token file: %s\n", err)
		}
	}()

	e := os.Environ()
	e = append(e, fmt.Sprintf("GOOGLE_APPLICATION_CREDENTIALS=%s", keyPath))
	runner := NewEnviron(workspace.Path, e, os.Stdout, os.Stderr)

	err = runner.Run(vargs.GCloudCmd, "auth", "activate-service-account", "--key-file", keyPath)
	if err != nil {
		return fmt.Errorf("error: %s\n", err)
	}

	err = runner.Run(vargs.GCloudCmd, "container", "clusters", "get-credentials", vargs.Cluster, "--project", vargs.Project, "--zone", vargs.Zone)
	if err != nil {
		return fmt.Errorf("error: %s\n", err)
	}

	data := map[string]interface{}{

		// http://readme.drone.io/usage/variables/#string-interpolation:2b8b8ac4006be88c769f5e3fd99b009a
		"BUILD_NUMBER": build.Number,
		"COMMIT":       build.Commit,
		"BRANCH":       build.Branch,
		"TAG":          "", // How?

		// https://godoc.org/github.com/drone/drone-plugin-go/plugin#Workspace

		"workspace": workspace,
		"repo":      repo,
		"build":     build,
		"system":    system,

		// Misc useful stuff. Note that we don't include all of the vargs, since
		// that includes the GCP token.
		"project": vargs.Project,
		"cluster": vargs.Cluster,
	}

	for k, v := range vargs.Vars {
		// Don't allow vars to be overriden. We do this to ensure that the
		// built-in template vars (above) can be relied upon.
		if _, ok := data[k]; ok {
			return fmt.Errorf("var %q shadows existing var\n", k)
		}

		data[k] = v
	}

	if vargs.Verbose {
		dump := data
		delete(dump, "workspace")
		dumpData(os.Stdout, "DATA (Workspace Values Omitted)", dump)
	}

	secrets := map[string]interface{}{}
	for k, v := range vargs.Secrets {
		// Base64 encode secret strings.
		secrets[k] = base64.StdEncoding.EncodeToString([]byte(v.(string)))
	}

	// TODO: infer these filenames & check for presence
	mapping := map[string]map[string]interface{}{
		vargs.Template:       data,
		vargs.SecretTemplate: secrets,
	}

	outPaths := make(map[string]string)
	pathArg := []string{}

	for t, content := range mapping {
		if t == "" {
			continue
		}

		inPath := filepath.Join(workspace.Path, t)
		bn := filepath.Base(inPath)

		// Generate the file
		blob, err := ioutil.ReadFile(inPath)
		if err != nil {
			return fmt.Errorf("error reading template: %s\n", err)
		}

		tmpl, err := template.New(bn).Option("missingkey=error").Parse(string(blob))
		if err != nil {
			return fmt.Errorf("error parsing template: %s\n", err)
		}

		outPaths[t] = fmt.Sprintf("/tmp/%s", bn)
		f, err := os.Create(outPaths[t])
		if err != nil {
			return fmt.Errorf("error creating deployment file: %s\n", err)
		}

		err = tmpl.Execute(f, content)
		if err != nil {
			return fmt.Errorf("error executing deployment template: %s\n", err)
		}

		f.Close()

		pathArg = append(pathArg, outPaths[t])
	}

	if vargs.Verbose {
		dumpFile(os.Stdout, "DEPLOYMENT (Secret Template Omitted)", outPaths[vargs.Template])
	}

	if vargs.DryRun {
		fmt.Printf("skipping kubectl apply, because dry_run=true\n")
		return nil
	}

	err = runner.Run(vargs.KubectlCmd, "apply", "--filename", strings.Join(pathArg, ","))
	if err != nil {
		return fmt.Errorf("error: %s\n", err)
	}

	return nil
}

type token struct {
	ProjectID string `json:"project_id"`
}

func getProjectFromToken(j string) string {
	t := token{}
	err := json.Unmarshal([]byte(j), &t)
	if err != nil {
		return ""
	}
	return t.ProjectID
}