class Iterion < Formula
  desc "Build, run and orchestrate agentic AI workflows, from readable .bot files"
  homepage "https://github.com/SocialGouv/iterion"
  version "4.7.1"
  license "MIT"

  on_macos do
    on_arm do
      url "https://github.com/SocialGouv/iterion/releases/download/v#{version}/iterion-darwin-arm64"
      sha256 "9fbd306073c37751e4deb7197e7349aac9defe60a110bd1d75ce666f64c49350"
    end
    on_intel do
      url "https://github.com/SocialGouv/iterion/releases/download/v#{version}/iterion-darwin-amd64"
      sha256 "c64b097bfc3d1d6dae208a6d65244108976d72ba16e2a3aee589b1bd286a240a"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/SocialGouv/iterion/releases/download/v#{version}/iterion-linux-arm64"
      sha256 "ec364e9ffa3c09edddb24b4be6a52f993f129ffb4d42a2703d33e5a62e72f671"
    end
    on_intel do
      url "https://github.com/SocialGouv/iterion/releases/download/v#{version}/iterion-linux-amd64"
      sha256 "18068a66ba22ab4cc67b5d84e5c569f0a091b2218e1149101e66701979f4cc69"
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
