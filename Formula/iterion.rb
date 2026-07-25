class Iterion < Formula
  desc "Workflow orchestration engine with a custom DSL (.bot files)"
  homepage "https://github.com/SocialGouv/iterion"
  version "3.7.0"
  license "MIT"

  on_macos do
    on_arm do
      url "https://github.com/SocialGouv/iterion/releases/download/v#{version}/iterion-darwin-arm64"
      sha256 "d036a7db3cfe02833e4f8ee6bc06d73d597e9c0cde5f3912b13467e071ba119c"
    end
    on_intel do
      url "https://github.com/SocialGouv/iterion/releases/download/v#{version}/iterion-darwin-amd64"
      sha256 "fc8d91f3e18b7d6492864bfb0e3acc62ee7672fd44d0c2b2197301653cb8f3a3"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/SocialGouv/iterion/releases/download/v#{version}/iterion-linux-arm64"
      sha256 "1a620bc668b2f71aa8b84244897bf41aebc874b0e4a9e7e4e4e25e14a9aee096"
    end
    on_intel do
      url "https://github.com/SocialGouv/iterion/releases/download/v#{version}/iterion-linux-amd64"
      sha256 "585026dca4f8b8abc7da67b3a3d364ee8fc9cc2ca21fcdeeb5951c418a718ea6"
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
