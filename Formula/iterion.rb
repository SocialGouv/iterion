class Iterion < Formula
  desc "Build, run and orchestrate agentic AI workflows, from readable .bot files"
  homepage "https://github.com/SocialGouv/iterion"
  version "3.232.1"
  license "MIT"

  on_macos do
    on_arm do
      url "https://github.com/SocialGouv/iterion/releases/download/v#{version}/iterion-darwin-arm64"
      sha256 "d50aba3092c2817410d3aa52b1f328b6b790cc9f6076c4ad865ad0c4debbb448"
    end
    on_intel do
      url "https://github.com/SocialGouv/iterion/releases/download/v#{version}/iterion-darwin-amd64"
      sha256 "0bdbe676bb0ea3aa9f814f27cdf43a44e91f653a55959c0f63ac7a0cf469984d"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/SocialGouv/iterion/releases/download/v#{version}/iterion-linux-arm64"
      sha256 "a541819d458e12a69df59c2177f99026bbbabe04b0e4b9896ec32b17b9747834"
    end
    on_intel do
      url "https://github.com/SocialGouv/iterion/releases/download/v#{version}/iterion-linux-amd64"
      sha256 "8cc184a7adc7eb85fc4204c2fe396ea93f641cb87465100fe152b08102321f29"
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
