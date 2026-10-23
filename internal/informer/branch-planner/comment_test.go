package branchplanner

import (
	"testing"

	gom "github.com/onsi/gomega"

	"github.com/flux-iac/tofu-controller/internal/config"
)

func Test_formatPlanOutput(t *testing.T) {
	g := gom.NewWithT(t)

	planOutput := "OpenTofu will perform the following actions:\n\nPlan: 1 to add, 0 to change, 0 to destroy.\n"
	content, err := formatPlanOutput(config.CommentSubject("tf-components", "mtp-vpc", "sandbox-us-east-1-main"), shortRevision("refs/heads/develop@sha1:6437813c13c43e45dff41231f68a4e5b65d7d062"), planOutput)
	g.Expect(err).NotTo(gom.HaveOccurred())

	g.Expect(string(content)).To(gom.HavePrefix("#### Plan for `tf-components/mtp-vpc` on `sandbox-us-east-1-main` at `6437813`\n"))
	g.Expect(string(content)).To(gom.ContainSubstring("**Plan: 1 to add, 0 to change, 0 to destroy.**"))
	g.Expect(string(content)).To(gom.ContainSubstring("<details>\n<summary>Show plan output</summary>\n\n```hcl\n" + planOutput + "\n```\n\n</details>"))

	content, err = formatPlanOutput(config.CommentSubject("tf-components", "mtp-vpc", ""), "", "No changes. Your infrastructure matches the configuration.\n")
	g.Expect(err).NotTo(gom.HaveOccurred())

	g.Expect(string(content)).To(gom.HavePrefix("#### Plan for `tf-components/mtp-vpc`\n"))
	g.Expect(string(content)).To(gom.ContainSubstring("**No changes. Your infrastructure matches the configuration.**"))
}

func Test_formatErrorOutput(t *testing.T) {
	g := gom.NewWithT(t)

	content, err := formatErrorOutput(config.CommentSubject("tf-components", "mtp-vpc", "sandbox-us-east-1-main"), "", "error running Init")
	g.Expect(err).NotTo(gom.HaveOccurred())

	g.Expect(string(content)).To(gom.HavePrefix("#### Plan failed for `tf-components/mtp-vpc` on `sandbox-us-east-1-main`\n"))
	g.Expect(string(content)).To(gom.ContainSubstring("<details>\n<summary>Show error</summary>\n\n```\nerror running Init\n```\n\n</details>"))
}
