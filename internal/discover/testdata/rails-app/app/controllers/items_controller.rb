class ItemsController < ApplicationController
  def index
    render json: []
  end

  def update
    head :no_content
  end
end
