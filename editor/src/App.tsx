import { Route, Switch } from "wouter";

import EditorView from "@/components/EditorView";
import LaunchView from "@/components/Runs/LaunchView";
import RunListView from "@/components/Runs/RunListView";
import RunView from "@/components/Runs/RunView";
import ToastContainer from "@/components/shared/Toast";

export default function App() {
  return (
    <>
      <Switch>
        <Route path="/runs/new" component={LaunchView} />
        <Route path="/runs/:id" component={RunView} />
        <Route path="/runs" component={RunListView} />
        <Route component={EditorView} />
      </Switch>
      <ToastContainer />
    </>
  );
}
