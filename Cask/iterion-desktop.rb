cask "iterion-desktop" do
  version "3.112.9"
  sha256 "03791626017a3668f5e3022eedfc7d1e0ecdfe5c0f83c8aac64a44b0884cce10"

  url "https://github.com/SocialGouv/iterion/releases/download/v#{version}/iterion-desktop-darwin-universal.zip"
  name "Iterion Desktop"
  desc "Workflow orchestration engine — desktop app"
  homepage "https://github.com/SocialGouv/iterion"

  livecheck do
    url :url
    strategy :github_latest
  end

  app "Iterion.app"

  zap trash: [
    "~/Library/Application Support/Iterion",
    "~/Library/Caches/Iterion",
    "~/Library/Logs/Iterion",
    "~/Library/Preferences/com.iterion.Iterion.plist",
    "~/Library/Saved Application State/com.iterion.Iterion.savedState",
  ]
end
