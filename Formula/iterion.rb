class Iterion < Formula
  desc "Workflow orchestration engine with a custom DSL (.iter files)"
  homepage "https://github.com/SocialGouv/iterion"
  version "0.8.0"
  license "MIT"

  on_macos do
    on_arm do
      url "https://github.com/SocialGouv/iterion/releases/download/v#{version}/iterion-darwin-arm64"
      sha256 "ce5fc6e8e21db2491756e1e44d04eb72e1db21e34b828dfaf0c2e35b5315e605"
    end
    on_intel do
      url "https://github.com/SocialGouv/iterion/releases/download/v#{version}/iterion-darwin-amd64"
      sha256 "c2d82400e64933bdfd21ba9a260f11d0d9439f7fdc3fababbc7cee3b5ad3e995"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/SocialGouv/iterion/releases/download/v#{version}/iterion-linux-arm64"
      sha256 "e1fa50479b0184e0e10a8fa7f2d61987d87bb4cd5b919b9c8a0fab9c723e788d"
    end
    on_intel do
      url "https://github.com/SocialGouv/iterion/releases/download/v#{version}/iterion-linux-amd64"
      sha256 "8e736f5be13d0cceaf0164b89bf191fc368f1651feba617786fd28c8dd939bfd"
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
