cask "iterion-desktop" do
  version "3.127.2"
  sha256 "898b5d4f76418fd63964441c1b5dfac32f81eba2f1a98f5323cf7d443b7db890"

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
