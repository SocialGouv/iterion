class Iterion < Formula
  desc "Workflow orchestration engine with a custom DSL (.iter files)"
  homepage "https://github.com/SocialGouv/iterion"
  version "0.6.0"
  license "MIT"

  on_macos do
    on_arm do
      url "https://github.com/SocialGouv/iterion/releases/download/v#{version}/iterion-darwin-arm64"
      sha256 "58c540773106b7960f777c1f3cc5a66baabb87f7c8b34f3a7f8d7e2a2afc9daf"
    end
    on_intel do
      url "https://github.com/SocialGouv/iterion/releases/download/v#{version}/iterion-darwin-amd64"
      sha256 "0a11951df6a9d9c5087cc5c286df5bf2b550e680d14579e9e52fc61b86513b3d"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/SocialGouv/iterion/releases/download/v#{version}/iterion-linux-arm64"
      sha256 "da828846a11ae5d172b4e8c3e0f6a84c0ad3457df36b1c8830acbb664069ce84"
    end
    on_intel do
      url "https://github.com/SocialGouv/iterion/releases/download/v#{version}/iterion-linux-amd64"
      sha256 "3814c4a76a6e9e8f11e94fe6a27b10d46fc09f5776ea63ef47c04373b03e6539"
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
