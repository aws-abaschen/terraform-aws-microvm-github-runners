// Command guardtest checks modules/org-guardrails live, inside a member
// account whose OU has the SCP, RCP and tag policy attached
// (tools/guardrails). It reaches the account through
// OrganizationAccountAccessRole (exempt), creates test principals with
// AdministratorAccess, so that only the organisation policies can deny,
// and expects each call to be allowed or denied. Everything it creates is
// deleted at the end.
//
//	just guardrails-test
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	ststypes "github.com/aws/aws-sdk-go-v2/service/sts/types"
)

const (
	idA, repoA = "3fa2c1b0d9", "acme/app"
	idB        = "9c01e2d3f4"
	admin      = "arn:aws:iam::aws:policy/AdministratorAccess"
)

var failed int

var skipped int

func check(name string, err error, allow bool) {
	// A new account activates its services over minutes to hours: a call to
	// a service that isn't active yet proves nothing either way.
	if err != nil && (strings.Contains(err.Error(), "OptInRequired") || strings.Contains(err.Error(), "SubscriptionRequired") || strings.Contains(err.Error(), "not created")) {
		want := "allow"
		if !allow {
			want = "deny"
		}
		skipped++
		fmt.Printf("SKIP  want %-5s %-55s %s\n", want, name, "service not active in the new account yet")
		return
	}
	denied := err != nil && isDenied(err)
	ok := (allow && err == nil) || (!allow && denied)
	mark, want := "PASS", "allow"
	if !allow {
		want = "deny"
	}
	if !ok {
		mark = "FAIL"
		failed++
	}
	note := "allowed"
	if err != nil {
		note = short(err)
	}
	fmt.Printf("%s  want %-5s %-55s %s\n", mark, want, name, note)
}

func isDenied(err error) bool {
	s := err.Error()
	return strings.Contains(s, "AccessDenied") || strings.Contains(s, "not authorized") || strings.Contains(s, "explicit deny")
}

func short(err error) string {
	s := err.Error()
	for _, k := range []string{"service control policy", "resource control policy", "explicit deny", "AccessDenied"} {
		if strings.Contains(s, k) {
			return "denied (" + k + ")"
		}
	}
	if i := strings.Index(s, "api error "); i >= 0 {
		s = s[i+len("api error "):]
	}
	if len(s) > 160 {
		s = s[:160]
	}
	return s
}

func main() {
	account := flag.String("account", "", "member account id")
	region := flag.String("region", "eu-central-1", "AWS Region")
	flag.Parse()
	if *account == "" {
		fmt.Fprintln(os.Stderr, "-account is required")
		os.Exit(2)
	}
	ctx := context.Background()
	mgmt, err := config.LoadDefaultConfig(ctx, config.WithRegion(*region))
	if err != nil {
		fail("aws config: %v", err)
	}
	iamArn := "arn:aws:iam::" + *account
	exempt := assume(mgmt, iamArn+":role/OrganizationAccountAccessRole", nil)
	adm := iam.NewFromConfig(exempt)
	suffix := fmt.Sprint(time.Now().Unix())
	trust := fmt.Sprintf(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":"arn:aws:iam::%s:root"},"Action":["sts:AssumeRole","sts:TagSession"]}]}`, *account)
	lambdaTrust := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"lambda.amazonaws.com"},"Action":"sts:AssumeRole"}]}`
	boundaryDoc := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"*","Resource":"*"}]}`
	repoTags := []iamtypes.Tag{{Key: aws.String("gha-repo-id"), Value: aws.String(idA)}, {Key: aws.String("gha-repo"), Value: aws.String(repoA)}}

	var cleanup []func()
	defer func() {
		for i := len(cleanup) - 1; i >= 0; i-- {
			cleanup[i]()
		}
		fmt.Printf("\n%d failed, %d skipped\n", failed, skipped)
		if failed > 0 {
			os.Exit(1)
		}
	}()
	mkRole := func(name, doc string, tags []iamtypes.Tag) {
		_, err := adm.CreateRole(ctx, &iam.CreateRoleInput{RoleName: aws.String(name), AssumeRolePolicyDocument: aws.String(doc), Tags: tags})
		check("exempt admin creates "+name, err, true)
		if err != nil {
			return
		}
		_, err = adm.AttachRolePolicy(ctx, &iam.AttachRolePolicyInput{RoleName: aws.String(name), PolicyArn: aws.String(admin)})
		if err != nil {
			fail("attach admin to %s: %v", name, err)
		}
		cleanup = append(cleanup, func() {
			_, _ = adm.DetachRolePolicy(ctx, &iam.DetachRolePolicyInput{RoleName: aws.String(name), PolicyArn: aws.String(admin)})
			_, _ = adm.DeleteRole(ctx, &iam.DeleteRoleInput{RoleName: aws.String(name)})
		})
	}

	fmt.Println("Setup, as the exempt OrganizationAccountAccessRole")
	repoRole := "guardtest-repo-a-" + suffix
	plainRole := "guardtest-plain-" + suffix
	target := "guardtest-target-" + suffix
	mkRole(repoRole, trust, repoTags)        // a repository principal with admin rights
	mkRole(plainRole, trust, nil)            // an untagged principal with admin rights
	mkRole("gha-microvm-tagger", trust, nil) // the platform tagger (exempt from the tag rules)
	mkRole(target, trust, nil)               // a role to assume with session tags
	boundaryArn := iamArn + ":policy/gha-microvm-boundary"
	deletePolicy := func() {
		if v, err := adm.ListPolicyVersions(ctx, &iam.ListPolicyVersionsInput{PolicyArn: aws.String(boundaryArn)}); err == nil {
			for _, pv := range v.Versions {
				if !pv.IsDefaultVersion {
					_, _ = adm.DeletePolicyVersion(ctx, &iam.DeletePolicyVersionInput{PolicyArn: aws.String(boundaryArn), VersionId: pv.VersionId})
				}
			}
		}
		_, _ = adm.DeletePolicy(ctx, &iam.DeletePolicyInput{PolicyArn: aws.String(boundaryArn)})
	}
	deletePolicy() // a leftover from an interrupted run
	_, err = adm.CreatePolicy(ctx, &iam.CreatePolicyInput{PolicyName: aws.String("gha-microvm-boundary"), PolicyDocument: aws.String(boundaryDoc)})
	check("exempt admin creates the boundary policy", err, true)
	cleanup = append(cleanup, deletePolicy)
	// Data for the RCP checks: queues tagged for A and for B (SQS is one of
	// the RCP's services and is available as soon as an account is created).
	data := func(name, id string) string {
		out, err := sqs.NewFromConfig(exempt).CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: aws.String(name), Tags: map[string]string{"gha-repo-id": id}})
		check("exempt admin creates a data queue tagged "+id, err, true)
		if err != nil {
			return ""
		}
		cleanup = append(cleanup, func() {
			_, _ = sqs.NewFromConfig(exempt).DeleteQueue(ctx, &sqs.DeleteQueueInput{QueueUrl: out.QueueUrl})
		})
		return aws.ToString(out.QueueUrl)
	}
	dataA := data("guardtest-data-a-"+suffix, idA)
	dataB := data("guardtest-data-b-"+suffix, idB)
	fmt.Println("Waiting 15s for IAM to propagate")
	time.Sleep(15 * time.Second)

	repo := assume(exempt, iamArn+":role/"+repoRole, nil)
	plain := assume(exempt, iamArn+":role/"+plainRole, nil)
	tagger := assume(exempt, iamArn+":role/gha-microvm-tagger", nil)

	fmt.Println("\nSCP: repository tags only carry the caller's own values (IAM role tags)")
	ri := iam.NewFromConfig(repo)
	tagRole := func(c *iam.Client, role, id string) error {
		_, err := c.TagRole(ctx, &iam.TagRoleInput{RoleName: aws.String(role), Tags: []iamtypes.Tag{{Key: aws.String("gha-repo-id"), Value: aws.String(id)}}})
		return err
	}
	untagged := "guardtest-tag-n-" + suffix
	taggedB := "guardtest-tag-b-" + suffix
	mkRole(untagged, trust, nil)
	mkRole(taggedB, trust, []iamtypes.Tag{{Key: aws.String("gha-repo-id"), Value: aws.String(idB)}})
	time.Sleep(10 * time.Second)
	check("repo A tags an untagged role with its own id", tagRole(ri, untagged, idA), true)
	check("repo A tags a role with B's id", tagRole(ri, untagged, idB), false)
	_, err = ri.UntagRole(ctx, &iam.UntagRoleInput{RoleName: aws.String(untagged), TagKeys: []string{"gha-repo-id"}})
	check("repo A removes the gha-repo-id tag", err, false)
	check("repo A retags B's role with its own id", tagRole(ri, taggedB, idA), false)
	check("untagged principal tags a role with A's id", tagRole(iam.NewFromConfig(plain), taggedB, idA), false)
	check("exempt admin retags B's role", tagRole(adm, taggedB, idB), true)

	fmt.Println("\nSCP: the same on SQS queues")
	rq := sqs.NewFromConfig(repo)
	q, err := rq.CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: aws.String("guardtest-a-" + suffix), Tags: map[string]string{"gha-repo-id": idA}})
	check("repo A creates a queue tagged with its own id", err, true)
	if err == nil {
		cleanup = append(cleanup, func() { _, _ = sqs.NewFromConfig(exempt).DeleteQueue(ctx, &sqs.DeleteQueueInput{QueueUrl: q.QueueUrl}) })
		_, err = rq.TagQueue(ctx, &sqs.TagQueueInput{QueueUrl: q.QueueUrl, Tags: map[string]string{"gha-repo-id": idB}})
		check("repo A retags its queue with B's id", err, false)
		pq, perr := sqs.NewFromConfig(exempt).CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: aws.String("guardtest-ob-" + suffix), Tags: map[string]string{"gha-repo-id": idB}})
		check("exempt admin creates a queue tagged with B's id", perr, true)
		if perr == nil {
			cleanup = append(cleanup, func() {
				_, _ = sqs.NewFromConfig(exempt).DeleteQueue(ctx, &sqs.DeleteQueueInput{QueueUrl: pq.QueueUrl})
			})
			_, err = rq.TagQueue(ctx, &sqs.TagQueueInput{QueueUrl: pq.QueueUrl, Tags: map[string]string{"gha-repo-id": idA}})
			check("repo A retags B's queue with its own id", err, false)
		}
		_, err = rq.UntagQueue(ctx, &sqs.UntagQueueInput{QueueUrl: q.QueueUrl, TagKeys: []string{"gha-repo-id"}})
		check("repo A removes the gha-repo-id tag", err, false)
	}
	_, err = rq.CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: aws.String("guardtest-b-" + suffix), Tags: map[string]string{"gha-repo-id": idB}})
	check("repo A creates a queue tagged with B's id", err, false)
	_, err = sqs.NewFromConfig(plain).CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: aws.String("guardtest-p-" + suffix), Tags: map[string]string{"gha-repo-id": idA}})
	check("untagged principal creates a queue tagged with A's id", err, false)

	fmt.Println("\nSCP: only the tagger sets repository session tags")
	sessionTags := []ststypes.Tag{{Key: aws.String("gha-repo-id"), Value: aws.String(idA)}, {Key: aws.String("gha-repo"), Value: aws.String(repoA)}}
	try := func(cfg aws.Config) error {
		_, err := sts.NewFromConfig(cfg).AssumeRole(ctx, &sts.AssumeRoleInput{RoleArn: aws.String(iamArn + ":role/" + target), RoleSessionName: aws.String("guardtest"), Tags: sessionTags})
		return err
	}
	check("tagger assumes a role with repository session tags", try(tagger), true)
	check("untagged principal assumes with repository session tags", try(plain), false)
	check("repo A assumes with its own repository session tags", try(repo), false)

	fmt.Println("\nSCP: boundary, escalation and platform roles")
	_, err = ri.CreateRole(ctx, &iam.CreateRoleInput{RoleName: aws.String("guardtest-nb-" + suffix), AssumeRolePolicyDocument: aws.String(lambdaTrust)})
	check("repo A creates a role without the boundary", err, false)
	name := "guardtest-wb-" + suffix
	_, err = ri.CreateRole(ctx, &iam.CreateRoleInput{RoleName: aws.String(name), AssumeRolePolicyDocument: aws.String(lambdaTrust), PermissionsBoundary: aws.String(boundaryArn), Tags: repoTags})
	check("repo A creates a role with the boundary and its tags", err, true)
	if err == nil {
		cleanup = append(cleanup, func() { _, _ = adm.DeleteRole(ctx, &iam.DeleteRoleInput{RoleName: aws.String(name)}) })
		_, err = ri.DeleteRolePermissionsBoundary(ctx, &iam.DeleteRolePermissionsBoundaryInput{RoleName: aws.String(name)})
		check("repo A removes that role's boundary", err, false)
	}
	_, err = ri.CreateAccessKey(ctx, &iam.CreateAccessKeyInput{UserName: aws.String("guardtest-nobody")})
	check("repo A creates an access key", err, false)
	upd := func(c aws.Config) error {
		_, err := iam.NewFromConfig(c).UpdateAssumeRolePolicy(ctx, &iam.UpdateAssumeRolePolicyInput{RoleName: aws.String("gha-microvm-tagger"), PolicyDocument: aws.String(trust)})
		return err
	}
	check("repo A edits the tagger role's trust", upd(repo), false)
	check("untagged principal edits the tagger role's trust", upd(plain), false)
	check("exempt admin edits the tagger role's trust", upd(exempt), true)
	_, err = ri.CreatePolicyVersion(ctx, &iam.CreatePolicyVersionInput{PolicyArn: aws.String(boundaryArn), PolicyDocument: aws.String(boundaryDoc), SetAsDefault: true})
	check("repo A rewrites the boundary policy", err, false)

	fmt.Println("\nSCP: no MicroVM shell, even for administrators")
	shell := func(c aws.Config) error {
		_, err := lambdamicrovms.NewFromConfig(c).CreateMicrovmShellAuthToken(ctx, &lambdamicrovms.CreateMicrovmShellAuthTokenInput{
			MicrovmIdentifier: aws.String("arn:aws:lambda:" + *region + ":" + *account + ":microvm:guardtest"), ExpirationInMinutes: aws.Int32(5)})
		return err
	}
	// The service looks the VM up before it authorizes: without a running VM
	// in this account the answer is ResourceNotFound, which proves nothing.
	if err := shell(exempt); err != nil && strings.Contains(err.Error(), "ResourceNotFound") {
		skipped++
		fmt.Printf("SKIP  want deny  %-55s %s\n", "exempt admin asks for a shell token", "inconclusive: no VM to ask for (covered by just iam-sim)")
	} else {
		check("exempt admin asks for a shell token", err, false)
	}

	fmt.Println("\nRCP: a repository principal can't read data tagged for another")
	get := func(c aws.Config, url string) error {
		if url == "" {
			return errors.New("queue not created")
		}
		_, err := sqs.NewFromConfig(c).ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: aws.String(url), WaitTimeSeconds: 0})
		return err
	}
	check("repo A reads a queue tagged A", get(repo, dataA), true)
	check("repo A reads a queue tagged B (admin identity policy)", get(repo, dataB), false)
	check("untagged principal reads a queue tagged B", get(plain, dataB), true)
}

func assume(base aws.Config, role string, tags []ststypes.Tag) aws.Config {
	c := base.Copy()
	c.Credentials = aws.NewCredentialsCache(stscreds.NewAssumeRoleProvider(sts.NewFromConfig(base), role, func(o *stscreds.AssumeRoleOptions) {
		o.RoleSessionName = "guardtest"
		o.Tags = tags
	}))
	// Retry while a new role propagates.
	for i := 0; i < 12; i++ {
		if _, err := c.Credentials.Retrieve(context.Background()); err == nil {
			return c
		} else if i == 11 {
			fail("assume %s: %v", role, err)
		}
		time.Sleep(5 * time.Second)
	}
	return c
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(1)
}
