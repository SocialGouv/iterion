class Iterion < Formula
  desc "Workflow orchestration engine with a custom DSL (.iter files)"
  homepage "https://github.com/SocialGouv/iterion"
  version "0.14.0"
  license "MIT"

  on_macos do
    on_arm do
      url "https://github.com/SocialGouv/iterion/releases/download/v#{version}/iterion-darwin-arm64"
      sha256 "7985c23ba586c5e779cd27199f944f6e09f39ead2993e4326524f79d62e9bad1"
    end
    on_intel do
      url "https://github.com/SocialGouv/iterion/releases/download/v#{version}/iterion-darwin-amd64"
      sha256 "938319b62143a6194f05e6851e3cfa32435754ff23838e7ef0b6f7c9f9f971cd"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/SocialGouv/iterion/releases/download/v#{version}/iterion-linux-arm64"
      sha256 "9a1533c4da42213c5b6c7eb90c90fd75a60dd95e58e917e7e1201d32e90118af"
    end
    on_intel do
      url "https://github.com/SocialGouv/iterion/releases/download/v#{version}/iterion-linux-amd64"
      sha256 "42149446bea70b5f3a9098ddf99556adc6f2b161750c4febaf4fbc746d3e50b7"
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
