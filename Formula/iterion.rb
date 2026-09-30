class Iterion < Formula
  desc "Build, run and orchestrate agentic AI workflows, from readable .bot files"
  homepage "https://github.com/SocialGouv/iterion"
  version "3.218.6"
  license "MIT"

  on_macos do
    on_arm do
      url "https://github.com/SocialGouv/iterion/releases/download/v#{version}/iterion-darwin-arm64"
      sha256 "67ee2bcd3b0a1aa931bb45b2211f3ec9a8edf5c3ce977693bc2aa70d3be913fe"
    end
    on_intel do
      url "https://github.com/SocialGouv/iterion/releases/download/v#{version}/iterion-darwin-amd64"
      sha256 "7c65e10df03eecca115353f263520d7aa31f284e6501f58eb544b7d3d5fd8c4d"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/SocialGouv/iterion/releases/download/v#{version}/iterion-linux-arm64"
      sha256 "29351d274c0f516319ecbf6a0838ea41b1e1d04539bca9b1c0c6aa0703336384"
    end
    on_intel do
      url "https://github.com/SocialGouv/iterion/releases/download/v#{version}/iterion-linux-amd64"
      sha256 "de3ea91dc4efe89fb79edcb8a520b1846d34e64021e7631f2f01aace8c30666d"
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
