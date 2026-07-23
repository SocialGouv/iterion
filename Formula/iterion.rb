class Iterion < Formula
  desc "Workflow orchestration engine with a custom DSL (.bot files)"
  homepage "https://github.com/SocialGouv/iterion"
  version "3.1.0"
  license "MIT"

  on_macos do
    on_arm do
      url "https://github.com/SocialGouv/iterion/releases/download/v#{version}/iterion-darwin-arm64"
      sha256 "8be71bdc019d2e632f7cc69afa113d0b73fb6bc6bffc9ee4d243dbb4cb9beb51"
    end
    on_intel do
      url "https://github.com/SocialGouv/iterion/releases/download/v#{version}/iterion-darwin-amd64"
      sha256 "daf05a9f9f0b44a7cf8f13638c8bf0d8956b9e2bf36c08440b071e1456a31f9a"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/SocialGouv/iterion/releases/download/v#{version}/iterion-linux-arm64"
      sha256 "cd5ef95f2312724800e2a3abe7861df75b55e6292065bdefc37d0bcaca5b36de"
    end
    on_intel do
      url "https://github.com/SocialGouv/iterion/releases/download/v#{version}/iterion-linux-amd64"
      sha256 "14c5efa76edebb6cbff7acbc1c67c5db2e8642b9f09519a5963a4007cefc689a"
    end
  end

  def install
    bin.install Dir["iterion-*"].first => "iterion"
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/iterion version")
  end

  livecheck do
    url :stable
    strategy :github_latest
  end
end
