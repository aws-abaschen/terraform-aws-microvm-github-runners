// Command iamsim checks the rendered IAM documents against AWS's own policy
// engine: iam:SimulateCustomPolicy for allow and deny cases of the account
// boundary and the SCP, and Access Analyzer ValidatePolicy for every
// document. Nothing is deployed; it needs credentials that may call those
// two read-only APIs.
//
//	just iam-sim   # terraform -chdir=tools/iamsim output -json policies | go run ./cmd/iamsim
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/accessanalyzer"
	aatypes "github.com/aws/aws-sdk-go-v2/service/accessanalyzer/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
)

type policies struct {
	Account     string `json:"account"`
	Region      string `json:"region"`
	Boundary    string `json:"boundary"`
	SharedTrust string `json:"shared_trust"`
	EntryTrust  string `json:"entry_trust"`
	SCP         string `json:"scp"`
	RCP         string `json:"rcp"`
}

// ctx values are a string or a []string.
type kv map[string]any

type tc struct {
	name, action, resource string
	ctx                    kv
	allow                  bool
	scp                    bool // evaluate the SCP (with an allow-all identity policy) instead of the boundary
}

const allowAll = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"*","Resource":"*"}]}`

// Expected findings that are part of the design, by document and issue code.
var expected = map[string]string{
	"entry_trust/SPECIFIC_GITHUB_REPO_AND_BRANCH_RECOMMENDED": "wildcard sub per owner is the design; the tagger checks the repository",
	"entry_trust/CONFIRM_AUDIENCE_CLAIM_TYPE":                 "aud uses StringEquals",
}

func main() {
	dump := flag.Bool("dump", false, "print the simulation requests as JSON and exit, without calling AWS")
	flag.Parse()
	var p policies
	if err := json.NewDecoder(os.Stdin).Decode(&p); err != nil {
		fail("read terraform output from stdin: %v", err)
	}
	if *dump {
		type req struct {
			Name     string `json:"name"`
			Policy   string `json:"policy"` // boundary, or scp (with an allow-all identity policy)
			Action   string `json:"action"`
			Resource string `json:"resource"`
			Context  kv     `json:"context"`
			Allow    bool   `json:"allow"`
		}
		var out []req
		for _, c := range append(boundaryCases(p.Account, p.Region), scpCases(p.Account)...) {
			pol := "boundary"
			if c.scp {
				pol = "scp"
			}
			out = append(out, req{c.name, pol, c.action, c.resource, c.ctx, c.allow})
		}
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"cases": out, "docs": p})
		return
	}
	ctx := context.Background()
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(p.Region))
	if err != nil {
		fail("aws config: %v", err)
	}
	failed := 0

	fmt.Printf("Boundary: %d of 6,144 characters\n\n", len(p.Boundary))
	if len(p.Boundary) > 6144 {
		failed++
	}

	fmt.Println("Access Analyzer ValidatePolicy")
	aa := accessanalyzer.NewFromConfig(cfg)
	docs := []struct {
		name, doc string
		typ       aatypes.PolicyType
		resource  aatypes.ValidatePolicyResourceType
	}{
		{"boundary", p.Boundary, aatypes.PolicyTypeIdentityPolicy, ""},
		{"shared_trust", p.SharedTrust, aatypes.PolicyTypeResourcePolicy, aatypes.ValidatePolicyResourceType("AWS::IAM::AssumeRolePolicyDocument")},
		{"entry_trust", p.EntryTrust, aatypes.PolicyTypeResourcePolicy, aatypes.ValidatePolicyResourceType("AWS::IAM::AssumeRolePolicyDocument")},
		{"scp", p.SCP, aatypes.PolicyTypeServiceControlPolicy, ""},
		{"rcp", p.RCP, aatypes.PolicyTypeResourceControlPolicy, ""},
	}
	for _, d := range docs {
		in := &accessanalyzer.ValidatePolicyInput{PolicyDocument: &d.doc, PolicyType: d.typ}
		if d.resource != "" {
			in.ValidatePolicyResourceType = d.resource
		}
		out, err := aa.ValidatePolicy(ctx, in)
		if err != nil {
			fail("validate %s: %v", d.name, err)
		}
		if len(out.Findings) == 0 {
			fmt.Printf("PASS  %-13s no findings\n", d.name)
		}
		for _, f := range out.Findings {
			code := aws.ToString(f.IssueCode)
			mark := "PASS"
			switch why, ok := expected[d.name+"/"+code]; {
			case ok:
				code += " (expected: " + why + ")"
			case f.FindingType == aatypes.ValidatePolicyFindingTypeError || f.FindingType == aatypes.ValidatePolicyFindingTypeSecurityWarning:
				mark = "FAIL"
				failed++
			default:
				mark = "NOTE"
			}
			fmt.Printf("%s  %-13s %s %s\n", mark, d.name, f.FindingType, code)
		}
	}

	cases := boundaryCases(p.Account, p.Region)
	cases = append(cases, scpCases(p.Account)...)
	fmt.Printf("\niam:SimulateCustomPolicy, %d cases\n", len(cases))
	client := iam.NewFromConfig(cfg)
	type res struct {
		c        tc
		decision string
		err      error
	}
	results := make([]res, len(cases))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for i, c := range cases {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			pol := []string{p.Boundary}
			if c.scp {
				pol = []string{allowAll, p.SCP}
			}
			out, err := client.SimulateCustomPolicy(ctx, &iam.SimulateCustomPolicyInput{
				PolicyInputList: pol, ActionNames: []string{c.action}, ResourceArns: []string{c.resource}, ContextEntries: entries(c.ctx),
			})
			r := res{c: c, err: err}
			if err == nil && len(out.EvaluationResults) == 1 {
				r.decision = string(out.EvaluationResults[0].EvalDecision)
			}
			results[i] = r
		}()
	}
	wg.Wait()
	for _, r := range results {
		want := "deny"
		if r.c.allow {
			want = "allow"
		}
		ok := r.err == nil && (r.decision == "allowed") == r.c.allow
		mark := "PASS"
		if !ok {
			mark = "FAIL"
			failed++
		}
		scope := "boundary"
		if r.c.scp {
			scope = "scp"
		}
		note := r.decision
		if r.err != nil {
			note = r.err.Error()
		}
		fmt.Printf("%s  %-8s want %-5s %-45s %s\n", mark, scope, want, r.c.name, note)
	}
	fmt.Printf("\n%d failed\n", failed)
	if failed > 0 {
		os.Exit(1)
	}
}

func entries(m kv) []iamtypes.ContextEntry {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]iamtypes.ContextEntry, 0, len(m))
	for _, k := range keys {
		e := iamtypes.ContextEntry{ContextKeyName: aws.String(k), ContextKeyType: iamtypes.ContextKeyTypeEnumString}
		switch v := m[k].(type) {
		case []string:
			e.ContextKeyType, e.ContextKeyValues = iamtypes.ContextKeyTypeEnumStringList, v
		case string:
			e.ContextKeyValues = []string{v}
		}
		out = append(out, e)
	}
	return out
}

// with merges context maps, later ones winning.
func with(ms ...kv) kv {
	out := kv{}
	for _, m := range ms {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

func boundaryCases(acct, reg string) []tc {
	const (
		a, ra = "3fa2c1b0d9", "acme/app"   // the caller's repository
		x, rx = "9c01e2d3f4", "acme/other" // another repository in the account
	)
	l := fmt.Sprintf("arn:aws:lambda:%s:%s", reg, acct)
	iamArn := "arn:aws:iam::" + acct
	sm := fmt.Sprintf("arn:aws:secretsmanager:%s:%s:secret", reg, acct)
	ssm := fmt.Sprintf("arn:aws:ssm:%s:%s:parameter", reg, acct)
	state := fmt.Sprintf("arn:aws:s3:::gha-microvm-tfstate-%s-%s", acct, reg)
	sqs := fmt.Sprintf("arn:aws:sqs:%s:%s", reg, acct)
	ddb := fmt.Sprintf("arn:aws:dynamodb:%s:%s:table", reg, acct)
	logs := fmt.Sprintf("arn:aws:logs:%s:%s:log-group:/aws/lambda-microvms", reg, acct)
	bnd := iamArn + ":policy/gha-microvm-boundary"
	role := func(n string) string { return iamArn + ":role/" + n }

	// Default caller: repository A's own deploy role, tagged with A.
	base := kv{"aws:PrincipalTag/gha-repo-id": a, "aws:PrincipalTag/gha-repo": ra, "aws:PrincipalArn": role("gha-microvm-deploy-" + a)}
	as := func(r string) kv { return kv{"aws:PrincipalArn": role(r)} }
	shared := as("gha-microvm-deploy")
	tags := func(id, repo string) kv {
		return kv{"aws:RequestTag/gha-repo-id": id, "aws:RequestTag/gha-repo": repo, "aws:TagKeys": []string{"gha-repo-id", "gha-repo"}}
	}
	idTag := func(id string) kv {
		return kv{"aws:RequestTag/gha-repo-id": id, "aws:TagKeys": []string{"gha-repo-id"}}
	}
	boundary := kv{"iam:PermissionsBoundary": bnd}
	c := func(name, action, resource string, extra kv, allow bool) tc {
		return tc{name: name, action: action, resource: resource, ctx: with(base, extra), allow: allow}
	}
	return []tc{
		c("own function update", "lambda:UpdateFunctionCode", l+":function:gha-microvm-"+a+"-dispatcher", nil, true),
		c("other repo's function update", "lambda:UpdateFunctionCode", l+":function:gha-microvm-"+x+"-dispatcher", nil, false),
		c("broker function update", "lambda:UpdateFunctionCode", l+":function:gha-microvm-broker", nil, false),
		c("tagger function update", "lambda:UpdateFunctionCode", l+":function:gha-microvm-tagger", nil, false),
		c("tagger replay table", "dynamodb:DeleteItem", ddb+"/gha-microvm-tagger-jti", nil, false),
		c("own RunMicrovm", "lambda:RunMicrovm", l+":microvm-image:gha-microvm-"+a+"-runner", nil, true),
		c("other repo's RunMicrovm", "lambda:RunMicrovm", l+":microvm-image:gha-microvm-"+x+"-runner", nil, false),
		c("create image with own tag", "lambda:CreateMicrovmImage", "*", idTag(a), true),
		c("create image with other tag", "lambda:CreateMicrovmImage", "*", idTag(x), false),
		c("create own role", "iam:CreateRole", role("gha-microvm-"+a+"-runner"), with(boundary, tags(a, ra)), true),
		c("create role, other id tag", "iam:CreateRole", role("gha-microvm-"+a+"-runner"), with(boundary, tags(x, ra)), false),
		c("create role, other repo tag", "iam:CreateRole", role("gha-microvm-"+a+"-runner"), with(boundary, tags(a, rx)), false),
		c("create role without tags", "iam:CreateRole", role("gha-microvm-"+a+"-runner"), boundary, false),
		c("create role without boundary", "iam:CreateRole", role("gha-microvm-"+a+"-runner"), tags(a, ra), false),
		c("create role named for other repo", "iam:CreateRole", role("gha-microvm-"+x+"-runner"), with(boundary, tags(a, ra)), false),
		c("retag own role to other repo", "iam:TagRole", role("gha-microvm-"+a+"-runner"), idTag(x), false),
		c("untag own role", "iam:UntagRole", role("gha-microvm-"+a+"-runner"), kv{"aws:TagKeys": []string{"gha-repo-id"}}, false),
		c("remove boundary", "iam:DeleteRolePermissionsBoundary", role("gha-microvm-"+a+"-runner"), nil, false),
		c("pass own role", "iam:PassRole", role("gha-microvm-"+a+"-runner"), nil, true),
		c("pass deploy role", "iam:PassRole", role("gha-microvm-deploy-"+a), nil, false),
		c("own job secret", "secretsmanager:GetSecretValue", sm+":gha/"+ra+"/token-AbCdEf", nil, true),
		c("other repo's job secret", "secretsmanager:GetSecretValue", sm+":gha/"+rx+"/token-AbCdEf", nil, false),
		c("secret tagged with own id", "secretsmanager:GetSecretValue", sm+":shared/x-AbCdEf", kv{"aws:ResourceTag/gha-repo-id": a}, true),
		c("secret tagged with other id", "secretsmanager:GetSecretValue", sm+":shared/x-AbCdEf", kv{"aws:ResourceTag/gha-repo-id": x}, false),
		c("GitHub App key", "secretsmanager:GetSecretValue", sm+":gha-microvm/platform/github-app-AbCdEf", nil, false),
		c("own platform secret", "secretsmanager:PutSecretValue", sm+":gha-microvm/"+a+"/webhook-secret-AbCdEf", nil, true),
		c("other repo's platform secret", "secretsmanager:GetSecretValue", sm+":gha-microvm/"+x+"/webhook-secret-AbCdEf", nil, false),
		c("own parameter", "ssm:PutParameter", ssm+"/gha-microvm/"+a+"/jit/r1", nil, true),
		c("other repo's parameter", "ssm:GetParameter", ssm+"/gha-microvm/"+x+"/jit/r1", nil, false),
		c("read own registry entry", "ssm:GetParameter", ssm+"/gha-microvm/registry/"+a, nil, true),
		c("write own registry entry", "ssm:PutParameter", ssm+"/gha-microvm/registry/"+a, nil, false),
		c("read other registry entry", "ssm:GetParameter", ssm+"/gha-microvm/registry/"+x, nil, false),
		c("own state object", "s3:PutObject", state+"/"+a+"/runners.tfstate", nil, true),
		c("other repo's state object", "s3:GetObject", state+"/"+x+"/runners.tfstate", nil, false),
		c("delete a version of own object", "s3:DeleteObjectVersion", state+"/"+a+"/runners.tfstate", nil, true),
		c("delete a version of another repo's object", "s3:DeleteObjectVersion", state+"/"+x+"/runners.tfstate", nil, false),
		c("list versions under own prefix", "s3:ListBucketVersions", state, kv{"s3:prefix": a + "/image/x.zip"}, true),
		c("list versions under another prefix", "s3:ListBucketVersions", state, kv{"s3:prefix": x + "/"}, false),
		c("own queue", "sqs:SendMessage", sqs+":gha-microvm-"+a+"-jobs", nil, true),
		c("other repo's queue", "sqs:ReceiveMessage", sqs+":gha-microvm-"+x+"-jobs", nil, false),
		c("own table", "dynamodb:PutItem", ddb+"/gha-microvm-"+a+"-state", nil, true),
		c("other repo's table", "dynamodb:Scan", ddb+"/gha-microvm-"+x+"-state", nil, false),
		c("own log group", "logs:CreateLogGroup", logs+"/gha-microvm-"+a+"-runtime", nil, true),
		c("other repo's log stream", "logs:GetLogEvents", logs+"/gha-microvm-"+x+"-runtime:log-stream:x", nil, false),
		c("tag on create (resource *) with own id", "lambda:TagResource", "*", idTag(a), true),
		c("tag on create (resource *) with other id", "lambda:TagResource", "*", idTag(x), false),
		c("tag other function with other id", "lambda:TagResource", l+":function:gha-microvm-"+x+"-webhook", idTag(x), false),
		c("MicroVM shell token", "lambda:CreateMicrovmShellAuthToken", l+":microvm-image:gha-microvm-"+a+"-runner", nil, false),
		c("invoke the broker", "lambda:InvokeFunction", l+":function:gha-microvm-broker", nil, true),
		c("write a dashboard named for itself", "cloudwatch:PutDashboard", "arn:aws:cloudwatch::"+acct+":dashboard/gha-microvm-"+a, nil, false),
		c("write the platform dashboard", "cloudwatch:PutDashboard", "arn:aws:cloudwatch::"+acct+":dashboard/gha-microvm-platform", nil, false),
		c("KMS encrypt with the platform key", "kms:Encrypt", "arn:aws:kms:"+reg+":"+acct+":key/11111111-2222-3333-4444-555555555555", nil, true),
		c("KMS re-encrypt with the platform key", "kms:ReEncryptFrom", "arn:aws:kms:"+reg+":"+acct+":key/11111111-2222-3333-4444-555555555555", nil, true),
		c("KMS decrypt with another key", "kms:Decrypt", "arn:aws:kms:"+reg+":"+acct+":key/99999999-2222-3333-4444-555555555555", nil, false),
		c("create a network interface (VPC Lambda)", "ec2:CreateNetworkInterface", "*", nil, true),
		c("attach a network interface", "ec2:AttachNetworkInterface", "*", nil, false),
		// The simulator doesn't map PassNetworkConnector to connector ARNs (even
		// an allow on "*" is implicitDeny there); real deploys prove the ARN case.
		c("pass the platform network connector", "lambda:PassNetworkConnector", "*", nil, true),
		c("read the platform network connector", "lambda:GetNetworkConnector", l+":network-connector:nc-00000000-0000-0000-0000-000000000000", nil, true),
		c("read a code signing config", "lambda:GetCodeSigningConfig", l+":code-signing-config:csc-00000000000000000", nil, true),
		c("read an event source mapping (destroy polls it)", "lambda:GetEventSourceMapping", l+":event-source-mapping:00000000-0000-0000-0000-000000000000", nil, true),
		c("delete an event source mapping of another repo", "lambda:DeleteEventSourceMapping", l+":event-source-mapping:00000000-0000-0000-0000-000000000000", kv{"lambda:FunctionArn": l + ":function:gha-microvm-" + x + "-dispatcher"}, false),
		c("invoke the tagger", "lambda:InvokeFunction", l+":function:gha-microvm-tagger", nil, false),
		c("runtime role, own function", "lambda:GetFunction", l+":function:gha-microvm-"+a+"-webhook", as("gha-microvm-"+a+"-control"), true),
		c("deploy-B retagged as A", "lambda:UpdateFunctionCode", l+":function:gha-microvm-"+a+"-dispatcher", as("gha-microvm-deploy-"+x), false),
		c("deploy-B retagged as A, state", "s3:PutObject", state+"/"+a+"/runners.tfstate", as("gha-microvm-deploy-"+x), false),
		c("runtime role of B retagged as A", "secretsmanager:GetSecretValue", sm+":gha/"+ra+"/token-AbCdEf", as("gha-microvm-"+x+"-runner"), false),
		c("unrelated role tagged as A", "sqs:SendMessage", sqs+":gha-microvm-"+a+"-jobs", as("some-app"), false),
		c("shared deploy, own function", "lambda:UpdateFunctionCode", l+":function:gha-microvm-"+a+"-dispatcher", shared, true),
		c("shared deploy, other function", "lambda:UpdateFunctionCode", l+":function:gha-microvm-"+x+"-dispatcher", shared, false),
		c("shared deploy, create own role", "iam:CreateRole", role("gha-microvm-"+a+"-runner"), with(shared, boundary, tags(a, ra)), true),
		c("shared deploy, create role for B", "iam:CreateRole", role("gha-microvm-"+x+"-runner"), with(shared, boundary, tags(x, rx)), false),
		c("shared deploy, own state", "s3:PutObject", state+"/"+a+"/runners.tfstate", shared, true),
		c("shared deploy, other state", "s3:GetObject", state+"/"+x+"/runners.tfstate", shared, false),
		c("shared deploy, own job secret", "secretsmanager:GetSecretValue", sm+":gha/"+ra+"/token-AbCdEf", shared, true),
		c("shared deploy, other job secret", "secretsmanager:GetSecretValue", sm+":gha/"+rx+"/token-AbCdEf", shared, false),
		c("shared deploy retags itself", "iam:TagRole", role("gha-microvm-deploy"), with(shared, idTag(x)), false),
		c("shared deploy edits its trust", "iam:UpdateAssumeRolePolicy", role("gha-microvm-deploy"), shared, false),
		c("entry role tagged as A", "lambda:UpdateFunctionCode", l+":function:gha-microvm-"+a+"-dispatcher", as("gha-microvm-deploy-entry"), false),
		c("tagger role tagged as A", "s3:PutObject", state+"/"+a+"/runners.tfstate", as("gha-microvm-tagger"), false),
	}
}

func scpCases(acct string) []tc {
	role := func(n string) string { return "arn:aws:iam::" + acct + ":role/" + n }
	deploy := role("gha-microvm-deploy")
	tags := kv{"aws:RequestTag/gha-repo-id": "3fa2c1b0d9", "aws:RequestTag/gha-repo": "acme/app", "aws:TagKeys": []string{"gha-repo-id", "gha-repo"}}
	repoA := kv{"aws:PrincipalTag/gha-repo-id": "3fa2c1b0d9", "aws:PrincipalTag/gha-repo": "acme/app"}
	admin := "arn:aws:iam::" + acct + ":role/aws-reserved/sso.amazonaws.com/eu-central-1/AWSReservedSSO_AdministratorAccess_abc"
	c := func(name, action, resource string, ctx kv, allow bool) tc {
		return tc{name: name, action: action, resource: resource, ctx: ctx, allow: allow, scp: true}
	}
	return []tc{
		c("tagger sets repository session tags", "sts:TagSession", deploy, with(tags, kv{"aws:PrincipalArn": role("gha-microvm-tagger")}), true),
		c("tagger assumes with tags", "sts:AssumeRole", deploy, with(tags, kv{"aws:PrincipalArn": role("gha-microvm-tagger")}), true),
		c("another role sets repository tags", "sts:TagSession", deploy, with(tags, kv{"aws:PrincipalArn": role("some-app")}), false),
		c("deploy-A sets gha-repo alone", "sts:TagSession", deploy, with(repoA, kv{"aws:RequestTag/gha-repo": "acme/app", "aws:TagKeys": []string{"gha-repo"}, "aws:PrincipalArn": role("gha-microvm-deploy-3fa2c1b0d9")}), false),
		c("other session tags stay allowed", "sts:TagSession", deploy, kv{"aws:RequestTag/team": "x", "aws:TagKeys": []string{"team"}, "aws:PrincipalArn": role("some-app")}, true),
		c("repo role edits shared deploy trust", "iam:UpdateAssumeRolePolicy", deploy, with(repoA, kv{"aws:PrincipalArn": role("gha-microvm-3fa2c1b0d9-control")}), false),
		c("shared deploy edits the entry role", "iam:PutRolePolicy", role("gha-microvm-deploy-entry"), kv{"aws:PrincipalArn": deploy}, false),
		c("administrator edits shared deploy", "iam:UpdateAssumeRolePolicy", deploy, kv{"aws:PrincipalArn": admin}, true),
		c("repo role creates role without boundary", "iam:CreateRole", role("gha-microvm-3fa2c1b0d9-x"), with(repoA, kv{"aws:PrincipalArn": deploy}), false),
		c("repo role creates access key", "iam:CreateAccessKey", "arn:aws:iam::"+acct+":user/x", with(repoA, kv{"aws:PrincipalArn": deploy}), false),
		c("MicroVM shell token", "lambda:CreateMicrovmShellAuthToken", "*", kv{"aws:PrincipalArn": admin}, false),
		c("repo A retags a resource of B", "lambda:TagResource", "arn:aws:lambda:eu-central-1:"+acct+":function:gha-microvm-9c01e2d3f4-webhook", with(repoA, kv{"aws:PrincipalArn": deploy, "aws:ResourceTag/gha-repo-id": "9c01e2d3f4", "aws:RequestTag/gha-repo-id": "3fa2c1b0d9", "aws:TagKeys": []string{"gha-repo-id"}}), false),
		c("repo A tags its own resource", "lambda:TagResource", "arn:aws:lambda:eu-central-1:"+acct+":function:gha-microvm-3fa2c1b0d9-webhook", with(repoA, kv{"aws:PrincipalArn": deploy, "aws:ResourceTag/gha-repo-id": "3fa2c1b0d9", "aws:RequestTag/gha-repo-id": "3fa2c1b0d9", "aws:TagKeys": []string{"gha-repo-id"}}), true),
	}
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(1)
}
