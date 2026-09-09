class Iterion < Formula
  desc "Workflow orchestration engine with a custom DSL (.bot files)"
  homepage "https://github.com/SocialGouv/iterion"
  version "3.126.0"
  license "MIT"

  on_macos do
    on_arm do
      url "https://github.com/SocialGouv/iterion/releases/download/v#{version}/iterion-darwin-arm64"
      sha256 "680fcb1704bbe6c0f48ba41d7e10842e119a03c43f50bc7c20129aa102dce8b6"
    end
    on_intel do
      url "https://github.com/SocialGouv/iterion/releases/download/v#{version}/iterion-darwin-amd64"
      sha256 "1f8d470eba425d7b1268e482afb53b571f3e0c66ab7b1c634ad7ac5ebbbe69ce"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/SocialGouv/iterion/releases/download/v#{version}/iterion-linux-arm64"
      sha256 "8ed3eafc7573ca0ca03e8d56f8b805dfeb07fe158bc0ce5c4f6596fc7f033966"
    end
    on_intel do
      url "https://github.com/SocialGouv/iterion/releases/download/v#{version}/iterion-linux-amd64"
      sha256 "ff031d6b3de7c8737d5e1faeb12332f32f2cac7c1b1acab0c39d1cf2169a0edb"
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
