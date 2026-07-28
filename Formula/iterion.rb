class Iterion < Formula
  desc "Workflow orchestration engine with a custom DSL (.bot files)"
  homepage "https://github.com/SocialGouv/iterion"
  version "3.10.2"
  license "MIT"

  on_macos do
    on_arm do
      url "https://github.com/SocialGouv/iterion/releases/download/v#{version}/iterion-darwin-arm64"
      sha256 "3d5c010e3575d9370741fe1c6e7fea462fcd32a4d0301701bc8b2f02c9e17566"
    end
    on_intel do
      url "https://github.com/SocialGouv/iterion/releases/download/v#{version}/iterion-darwin-amd64"
      sha256 "7d3986a9ab8f1e8b4d8a4e5ba8281b90b41ffb5ed7a450ff5a6eb40932c9b65d"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/SocialGouv/iterion/releases/download/v#{version}/iterion-linux-arm64"
      sha256 "002718af6e883ed253efb113fc9c5dee17d0b63c0eae5c9f4166ffa3568ed734"
    end
    on_intel do
      url "https://github.com/SocialGouv/iterion/releases/download/v#{version}/iterion-linux-amd64"
      sha256 "ffdd01ec8cc1a8cd3fa43b53b220b4c55c3c96db93e858c7e2c90f26adb73721"
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
