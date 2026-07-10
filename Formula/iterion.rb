class Iterion < Formula
  desc "Workflow orchestration engine with a custom DSL (.bot files)"
  homepage "https://github.com/SocialGouv/iterion"
  version "0.38.0"
  license "MIT"

  on_macos do
    on_arm do
      url "https://github.com/SocialGouv/iterion/releases/download/v#{version}/iterion-darwin-arm64"
      sha256 "9cceae9b4a97e4d5f907ccf11fe4befc10cbbc39b9da4ac58e1b1261cd5a9cf9"
    end
    on_intel do
      url "https://github.com/SocialGouv/iterion/releases/download/v#{version}/iterion-darwin-amd64"
      sha256 "b3f7c0f9c45b1d90b89ee49c0dedacdacb002f34c8d7001e06535a9a657b840b"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/SocialGouv/iterion/releases/download/v#{version}/iterion-linux-arm64"
      sha256 "6f5ebe68e5b74f69088d8f8d6bf912eeb3057bb639f498b1f2aaed2556e589fa"
    end
    on_intel do
      url "https://github.com/SocialGouv/iterion/releases/download/v#{version}/iterion-linux-amd64"
      sha256 "f8f21bbe7f9a91fde226203c9a2fdfec94e677177bfd45d6e19e29d0f7943121"
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
