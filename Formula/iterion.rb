class Iterion < Formula
  desc "Workflow orchestration engine with a custom DSL (.bot files)"
  homepage "https://github.com/SocialGouv/iterion"
  version "3.121.2"
  license "MIT"

  on_macos do
    on_arm do
      url "https://github.com/SocialGouv/iterion/releases/download/v#{version}/iterion-darwin-arm64"
      sha256 "2ce40dfb87ba2bcfe4462ed5c67d3d805d4166c3a7c45521d23b833d05c6ab32"
    end
    on_intel do
      url "https://github.com/SocialGouv/iterion/releases/download/v#{version}/iterion-darwin-amd64"
      sha256 "fd7cf2bd3e66bc624faa7d1d2d7ed1efcc1c86e0a811c1c5f428c88ae2ee626a"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/SocialGouv/iterion/releases/download/v#{version}/iterion-linux-arm64"
      sha256 "9f3e8e7c71b687e49e9a2b1347a46d1d65d4d32c4b75e8fbdb86f548bc8786dc"
    end
    on_intel do
      url "https://github.com/SocialGouv/iterion/releases/download/v#{version}/iterion-linux-amd64"
      sha256 "3cbe119f7ed41e7a1f2910e9a85aef5d664b7b69913c97c080ea4673966ebffc"
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
