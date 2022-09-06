# Developer Guide

Thank you for taking the time out to contribute to the **kube-vip** project!

This guide will walk you through the process of making your first commit and how
to effectively get it merged upstream.

<!-- toc -->
- [Getting Started](#getting-started)
- [Contribute](#contribute)
  - [GitHub Workflow](#github-workflow)
  - [Getting reviewers](#getting-reviewers)
  - [Inclusive Naming](#inclusive-naming)
  - [Building and testing your change](#building-and-testing-your-change)
  - [Reverting a commit](#reverting-a-commit)
  - [Sign-off Your Work](#sign-off-your-work)
- [Issue and PR Management](#issue-and-pr-management)
  - [Filing An Issue](#filing-an-issue)
  - [Issue Triage](#issue-triage)
<!-- /toc -->

## Getting Started

To get started, let's ensure you have completed the following prerequisites for
contributing to project **kube-vip**:

1. Read and observe the [code of conduct](CODE_OF_CONDUCT.md).
2. Check out the [Architecture documentation](https://kube-vip.io) for the **kube-vip**
   architecture and design.
3. Set up your [development environment](docs/contributors/manual-installation.md)

Now that you're setup, skip ahead to learn how to [contribute](#contribute).

**Also**, A GitHub account will be required in order to submit code changes and
interact with the project. For committing any changes in  **Github** it is required for
you to have a [github account](https://github.com/join).

## Contribute

There are multiple ways in which you can contribute, either by contributing
code in the form of new features or bug-fixes or non-code contributions like
helping with code reviews, trialing of bugs, documentation updates, filing
[new issues](#filing-an-issue) or writing blogs/manuals etc.

### GitHub Workflow

Developers work in their own forked copy of the repository and when ready,
submit pull requests to have their changes considered and merged into the
project's repository.

1. Fork your own copy of the repository to your GitHub account by clicking on
   `Fork` button on [kube-vip's GitHub repository](https://github.com/kube-vip/kube-vip).
2. Clone the forked repository on your local setup.

    ```bash
    git clone https://github.com/$user/kube-vip
    ```

    Add a remote upstream to track upstream kube-vip repository.

    ```bash
    git remote add upstream https://github.com/kube-vip/kube-vip
    ```

    Never push to upstream remote

    ```bash
    git remote set-url --push upstream no_push
    ```

3. Create a topic branch.

    ```bash
    git checkout -b branchName
    ```

4. Make changes and commit it locally. Make sure that your commit is
   [signed](#sign-off-your-work).

    ```bash
    git add <modifiedFile>
    git commit -s
    ```

5. Update the "Unreleased" section of the [CHANGELOG](CHANGELOG.md) for any
   significant change that impacts users.
6. Keeping branch in sync with upstream.

    ```bash
    git checkout branchName
    git fetch upstream
    git rebase upstream/main
    ```

7. Push local branch to your forked repository.

    ```bash
    git push -f $remoteBranchName branchName
    ```

8. Create a Pull request on GitHub.
   Visit your fork at `https://github.com/kube-vip/kube-vip` and click
   `Compare & Pull Request` button next to your `remoteBranchName` branch.

### Getting reviewers

Once you have opened a Pull Request (PR), reviewers will be assigned to your
PR and they may provide review comments which you need to address.
Commit changes made in response to review comments to the same branch on your
fork. Once a PR is ready to merge, squash any *fix review feedback, typo*
and *merged* sorts of commits.

To make it easier for reviewers to review your PR, consider the following:

1. Follow the golang [coding conventions](https://github.com/golang/go/wiki/CodeReviewComments).
2. Format your code with `make golangci-fix`; if the [linters](ci/README.md) flag an issue that
   cannot be fixed automatically, an error message will be displayed so you can address the issue.
3. Follow [git commit](https://chris.beams.io/posts/git-commit/) guidelines.
4. Follow [logging](https://github.com/kubernetes/community/blob/master/contributors/devel/sig-instrumentation/logging.md) guidelines.

If your PR fixes a bug or implements a new feature, add the appropriate test
cases to our [automated test suite](ci/README.md) to guarantee enough
coverage. A PR that makes significant code changes without contributing new test
cases will be flagged by reviewers and will not be accepted.

### Inclusive Naming

For symbol names and documentation, do not introduce new usage of harmful
language such as 'master / slave' (or 'slave' independent of 'master') and
'blacklist / whitelist'. For more information about what constitutes harmful
language and for a reference word replacement list, please refer to the
[Inclusive Naming Initiative](https://inclusivenaming.org/).

We are committed to removing all harmful language from the project. If you
detect existing usage of harmful language in code or documentation, please
report the issue to us or open a Pull Request to address it directly. Thanks!

### Building and testing your change

To build the **kube-vip** Docker image together with all **kube-vip** bits, you can simply
do:

1. Checkout your feature branch and `cd` into it.
2. Run `make dockerx86`

The second step will compile the **kube-vip** code in a `golang` container, and build
a `Ubuntu 20.04` Docker image that includes all the generated binaries. [`Docker`](https://docs.docker.com/install)
must be installed on your local machine in advance.

Alternatively, you can build the **kube-vip** code in your local Go environment. The
**kube-vip** project uses the [Go modules support](https://github.com/golang/go/wiki/Modules) which was introduced in Go 1.11. It
facilitates dependency tracking and no longer requires projects to live inside
the `$GOPATH`.

To develop locally, you can follow these steps:

 1. [Install Go 1.18](https://golang.org/doc/install)
 2. Checkout your feature branch and `cd` into it.
 3. To build all Go files and install them under `bin`, run `make bin`
 4. To run all Go unit tests, run `make test-unit`
 5. To build the **kube-vip** Ubuntu Docker image separately with the binaries generated in step 2, run `make ubuntu`

### CI testing

For more information about the tests we run as part of CI, please refer to
[ci/README.md](ci/README.md).

### Reverting a commit

1. Create a branch in your forked repo

    ```bash
    git checkout -b revertName
    ```

2. Sync the branch with upstream

    ```bash
    git fetch upstream
    git rebase upstream/main
    ```

3. Create a revert based on the SHA of the commit. The commit needs to be
   [signed](#sign-off-your-work).

    ```bash
    git revert -s SHA
    ```

4. Push this new commit.

    ```bash
    git push $remoteRevertName revertName
    ```

5. Create a Pull Request on GitHub.
   Visit your fork at `https://github.com/kube-vip/kube-vip` and click
   `Compare & Pull Request` button next to your `remoteRevertName` branch.

### Sign-off Your Work

It is recommended to sign your work when contributing to the **kube-vip**
repository. 

Git provides the `-s` command-line option to append the required line
automatically to the commit message:

```bash
git commit -s -m 'This is my commit message'
```

For an existing commit, you can also use this option with `--amend`:

```bash
git commit -s --amend
```

If more than one person works on something it's possible for more than one
person to sign-off on it. For example:

```bash
Signed-off-by: Some Developer somedev@example.com
Signed-off-by: Another Developer anotherdev@example.com
```

We use the [DCO Github App](https://github.com/apps/dco) to enforce that all
commits in a Pull Request include the required `Signed-off-by` line. If this is
not the case, the app will report a failed status for the Pull Request and it
will be blocked from being merged.

Compared to our earlier CLA, DCO tends to make the experience simpler for new
contributors. If you are contributing as an employee, there is no need for your
employer to sign anything; the DCO assumes you are authorized to submit
contributions (it's your responsibility to check with your employer).

## Issue and PR Management

We use labels and workflows (some manual, some automated with GitHub Actions) to
help us manage triage, prioritize, and track issue progress. For a detailed
discussion, see [docs/issue-management.md](docs/contributors/issue-management.md).

### Filing An Issue

Help is always appreciated. If you find something that needs fixing, please file
an issue [here](https://github.com/kube-vip/kube-vip/issues). Please ensure
that the issue is self explanatory and has enough information for an assignee to
get started.

Before picking up a task, go through the existing
[issues](https://github.com/kube-vip/kube-vip/issues) and make sure that your
change is not already being worked on. If it does not exist, please create a new
issue and discuss it with other members.

For simple contributions to **kube-vip**, please ensure that this minimum set of
labels are included on your issue:

* **kind** -- common ones are `kind/feature`, `kind/support`, `kind/bug`,
  `kind/documentation`, or `kind/design`. For an overview of the different types
  of issues that can be submitted, see [Issue and PR
  Kinds](#issue-and-pr-kinds).
  The kind of issue will determine the issue workflow.
* **area** (optional) -- if you know the area the issue belongs in, you can assign it.
  Otherwise, another community member will label the issue during triage. The
  area label will identify the area of interest an issue or PR belongs in and
  will ensure the appropriate reviewers shepherd the issue or PR through to its
  closure. For an overview of areas, see the
  [`docs/github-labels.md`](docs/contributors/github-labels.md).
* **size** (optional) -- if you have an idea of the size (lines of code, complexity,
  effort) of the issue, you can label it using a [size label](#size). The size
  can be updated during backlog grooming by contributors. This estimate is used
  to guide the number of features selected for a milestone.

All other labels will be assigned during issue triage.

### Issue Triage

Once an issue has been submitted, the CI (GitHub actions) or a human will
automatically review the submitted issue or PR to ensure that it has all relevant
information. If information is lacking or there is another problem with the
submitted issue, an appropriate `triage/<?>` label will be applied.

After an issue has been triaged, the maintainers can prioritize the issue with
an appropriate `priority/<?>` label.

Once an issue has been submitted, categorized, triaged, and prioritized it
is marked as `ready-to-work`. A ready-to-work issue should have labels
indicating assigned areas, prioritization, and should not have any remaining
triage labels.

