# Branch Planner Overview

The GitOps methodology streamlines infrastructure provisioning and management, using Git as the source of truth. The Branch Planner, a component of TF-Controller, aims to take this a step further by allowing developers and operations teams to plan Terraform configurations on a branch that's separate from the `main` branch. This makes it easier to review and understand the potential impact of your changes before you run `terraform apply`.

The Branch Planner's most important feature is its seamless integration with the Pull Request / Merge Request user interface. When enabled through Helm values, it watches repositories that contain Terraform resources at regular intervals—checking their referenced Source, and polling for Pull Requests (GitHub) or Merge Requests (GitLab) using the provider's API and the provided token. When changes are proposed on a new branch, Branch Planner runs a plan in the cluster and displays the results directly as comments on your PR/MR. Once you're satisfied with the results, you can merge your branch into the `main` branch to trigger the TF-Controller to reconcile the updated code.

Branch Planner supports **GitHub** (including GitHub Enterprise), **GitLab** (including self-hosted), **Bitbucket Cloud**, **Bitbucket Server**, **Gitea**, and **Azure DevOps** (experimental). The provider is automatically detected from the GitRepository source URL.

![branch planner](branch-planner.png)

### Base branch

Only Pull Requests whose base branch matches the branch tracked by the Terraform object's source (`.spec.ref.branch`, or `.spec.ref.name` of the form `refs/heads/<branch>`) are planned. Set `spec.branchPlanner.baseBranch` to override this. Sources that track a tag, semver range or commit need the override, otherwise no Pull Requests are planned.

### Plan comments

The Branch Planner keeps a single comment per Terraform object and cluster on each PR/MR, and updates it in place with every new plan (or plan error), showing the commit it was planned at. The comments are found by a hidden marker with the namespace and name of the Terraform object, and the cluster name set with the `branchPlanner.clusterName` Helm value (`--cluster-name` flag). Set a different cluster name in each cluster when several of them plan the same repository, otherwise they would overwrite each other's comments.

Bitbucket Cloud and Azure DevOps comments can't be edited, so a new comment is posted for each plan.

### Replan commands

The Branch Planner also allows users to manually trigger the replan process. By simply commenting `!replan` under the PR or MR, the Branch Planner will be instructed to generate a new plan and update its plan comments with it.

### File mappings

Files mapped into the workspace with `spec.fileMappings` are written on top of the source, so a Secret built from files in the tracked branch (e.g. tfvars) would hide the changes made to them by the PR/MR. For each Secret used by a `workspace` file mapping, the Branch Planner creates a copy for the PR/MR where the mapped files have their content in the PR/MR head, and deletes it when the PR/MR is closed.

By default, the file of a mapping is looked up in the repository at the path where the mapping writes it (`spec.path` joined with the mapping `path`). When the Secret is built from files in another directory, annotate the Secret with that directory, and the file of each key is looked up at `<dir>/<key>`:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: my-stack-tfvars
  annotations:
    infra.weave.works/branch-planner-source-dir: charts/my-stack/tfvars
```

Files missing from both the PR/MR head and its base keep the content of the Secret. Files deleted by the PR/MR are dropped from `spec.fileMappings`, together with the `spec.tfVarsFiles` entries pointing to them.

When the files of the Secret are selected from a set of candidates (e.g. per environment or region), list all of them as a JSON array, lowest precedence first. The candidates that exist in the PR/MR are then mapped to `<key>` in the workspace and loaded as `spec.tfVarsFiles`, in that order, so files added by the PR/MR are planned too:

```yaml
metadata:
  annotations:
    infra.weave.works/branch-planner-source-dir: charts/my-stack/tfvars
    infra.weave.works/branch-planner-source-files: '["default.tfvars", "prod.tfvars", "prod.us-east-1.tfvars"]'
```

The files are fetched from the git provider API again only when the PR/MR head, the Secret or the Terraform object change.

Now that you know what Branch Planner can do for you, follow the [guide to get started](./branch-planner-getting-started.md).

