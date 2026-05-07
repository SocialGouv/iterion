class Iterion < Formula
  desc "Workflow orchestration engine with a custom DSL (.iter files)"
  homepage "https://github.com/SocialGouv/iterion"
  version "0.5.4"
  license "MIT"

  on_macos do
    on_arm do
      url "https://github.com/SocialGouv/iterion/releases/download/v#{version}/iterion-darwin-arm64"
      sha256 "e95fb06bb0eddf017b03cb9c84b486d30a37dd0b0901eac6f899c2602bc0ea9e"
    end
    on_intel do
      url "https://github.com/SocialGouv/iterion/releases/download/v#{version}/iterion-darwin-amd64"
      sha256 "02a3840846bc469d6728482b3671d4da3442f403b1fcf69a4a9b58a2c0b154a8"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/SocialGouv/iterion/releases/download/v#{version}/iterion-linux-arm64"
      sha256 "e67a5c5f49a940759f0302326f7498e89bfbe33d741c86e537129e39fc0bea9d"
    end
    on_intel do
      url "https://github.com/SocialGouv/iterion/releases/download/v#{version}/iterion-linux-amd64"
      sha256 "24d1dde328821139aefab65f9341fd8932c73842c6612908a6ee154d67da9112"
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
