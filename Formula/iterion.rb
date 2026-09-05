class Iterion < Formula
  desc "Workflow orchestration engine with a custom DSL (.bot files)"
  homepage "https://github.com/SocialGouv/iterion"
  version "3.108.0"
  license "MIT"

  on_macos do
    on_arm do
      url "https://github.com/SocialGouv/iterion/releases/download/v#{version}/iterion-darwin-arm64"
      sha256 "f401df4ed6b9ad03201c2b805b19c29b7c9dc0ca1c6e3032be9f38657f15395e"
    end
    on_intel do
      url "https://github.com/SocialGouv/iterion/releases/download/v#{version}/iterion-darwin-amd64"
      sha256 "f1bd4f580ba27cfbc7af85e172ed96fff63b2deb34f728ba953c6b0f9b872194"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/SocialGouv/iterion/releases/download/v#{version}/iterion-linux-arm64"
      sha256 "4d0e3c3bd7c0e9b94120dc59a956efa5c85184752d4a3a5c98c3f6cb904e43dd"
    end
    on_intel do
      url "https://github.com/SocialGouv/iterion/releases/download/v#{version}/iterion-linux-amd64"
      sha256 "a2dc146d0040c1a1bf1f80f5b27ef65217048a26d5d5c2abb1e90873c5a48841"
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
