package prchecklist.services

import prchecklist.models._

import org.scalatest._

class GitHubPullRequestUtilsSpec extends FunSuite with Matchers {
  object utils extends GitHubPullRequestUtils

  test("mergedPullRequests") {
    val commits = List(
      """Merge pull request #1 from motemen/feature-1
        |
        |feature-1
      """.stripMargin,
      """Implement feature-1"""
    ).map {
        message =>
          GitHubTypes.Commit(
            sha = "x" * 40,
            commit = GitHubTypes.CommitDetail(message = message)
          )
      }

    utils.mergedPullRequests(commits) shouldBe List(
      PullRequestReference(1, "feature-1")
    )
  }
}
