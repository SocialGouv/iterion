class Iterion < Formula
  desc "Workflow orchestration engine with a custom DSL (.bot files)"
  homepage "https://github.com/SocialGouv/iterion"
  version "3.127.2"
  license "MIT"

  on_macos do
    on_arm do
      url "https://github.com/SocialGouv/iterion/releases/download/v#{version}/iterion-darwin-arm64"
      sha256 "03c5f158ddffbdbb596362837db926a3d90eee4e425bd0b1f2061092ff8b2e5c"
    end
    on_intel do
      url "https://github.com/SocialGouv/iterion/releases/download/v#{version}/iterion-darwin-amd64"
      sha256 "44e582f2644ac2617d71ed18d860e187ba4339fc7b5c3bb97629530f9b70ad80"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/SocialGouv/iterion/releases/download/v#{version}/iterion-linux-arm64"
      sha256 "5711c2b81dc8ba0483cbd535303e866ea3199102b31b8afdaf096422d104ce9b"
    end
    on_intel do
      url "https://github.com/SocialGouv/iterion/releases/download/v#{version}/iterion-linux-amd64"
      sha256 "4521900f186d51bb0809e59db87bf3b005280dff3123c5ba8cabbe1e87f6d859"
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
