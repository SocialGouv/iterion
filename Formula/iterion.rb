class Iterion < Formula
  desc "Build, run and orchestrate agentic AI workflows, from readable .bot files"
  homepage "https://github.com/SocialGouv/iterion"
  version "7.16.0"
  license "MIT"

  on_macos do
    on_arm do
      url "https://github.com/SocialGouv/iterion/releases/download/v#{version}/iterion-darwin-arm64"
      sha256 "76dc2e98b064ffac92f9e99a1adf7a80b9e9882ddb4e57f504854d9d1c506128"
    end
    on_intel do
      url "https://github.com/SocialGouv/iterion/releases/download/v#{version}/iterion-darwin-amd64"
      sha256 "149ac3d6ab154cb1f08def1b15b079e71284fce1e9cd323ec77529b8cb56fa85"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/SocialGouv/iterion/releases/download/v#{version}/iterion-linux-arm64"
      sha256 "6be62f6bdca0a1f2c4c8d3b2640658dfdfd207e5fb0e962d1a9d792eec57994c"
    end
    on_intel do
      url "https://github.com/SocialGouv/iterion/releases/download/v#{version}/iterion-linux-amd64"
      sha256 "c22e39c6621e642d4eed2deb3d20bfd5f2a305daccf19ff5dea290b368e96582"
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
