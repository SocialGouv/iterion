class Iterion < Formula
  desc "Build, run and orchestrate agentic AI workflows, from readable .bot files"
  homepage "https://github.com/SocialGouv/iterion"
  version "3.270.1"
  license "MIT"

  on_macos do
    on_arm do
      url "https://github.com/SocialGouv/iterion/releases/download/v#{version}/iterion-darwin-arm64"
      sha256 "d55e26fff5c61d73ccd1cd83c45a8ee2f13a8237f9b3a74134a7cde2f4caccb0"
    end
    on_intel do
      url "https://github.com/SocialGouv/iterion/releases/download/v#{version}/iterion-darwin-amd64"
      sha256 "2284c5bf9a58bb4bc102963c982a9c41eda19622ebc76789ce4efeda77d2dc08"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/SocialGouv/iterion/releases/download/v#{version}/iterion-linux-arm64"
      sha256 "6ba977946e769b8d9f9ffe96b4aa5c5a53bf50687015524d81a4d2ff9848e9a7"
    end
    on_intel do
      url "https://github.com/SocialGouv/iterion/releases/download/v#{version}/iterion-linux-amd64"
      sha256 "6aa16a2481c8de92fefcfc2893dc2d824f3771831879cf8bdaaa082845627e6e"
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
